package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Rules は録画ルールの永続化を PostgreSQL で実装する。
type Rules struct {
	pool      *pgxpool.Pool
	siteNames []string
}

var _ store.Rules = (*Rules)(nil)
var _ store.RuleImport = (*Rules)(nil)

// NewRules は PostgreSQL を使う Rules を作る。siteNames を省略した場合は default site を使う。
func NewRules(pool *pgxpool.Pool, siteNames ...string) *Rules {
	if len(siteNames) == 0 {
		siteNames = []string{db.DefaultSite}
	}
	return &Rules{pool: pool, siteNames: append([]string(nil), siteNames...)}
}

// List は子条件を含むルールを優先度降順、ID 昇順で返す。
func (s *Rules) List(ctx context.Context) ([]store.Rule, error) {
	q := sqlcgen.New(s.pool)
	rows, err := q.ListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	out := make([]store.Rule, 0, len(rows))
	for _, row := range rows {
		rule, err := loadRule(ctx, q, row)
		if err != nil {
			return nil, fmt.Errorf("loading rule %d: %w", row.ID, err)
		}
		out = append(out, rule)
	}
	return out, nil
}

// Get は子条件を含むルールを返す。
func (s *Rules) Get(ctx context.Context, id int64) (store.Rule, error) {
	q := sqlcgen.New(s.pool)
	row, err := q.GetRule(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Rule{}, fmt.Errorf("rule %d: %w", id, store.ErrNotFound)
		}
		return store.Rule{}, fmt.Errorf("getting rule %d: %w", id, err)
	}
	rule, err := loadRule(ctx, q, row)
	if err != nil {
		return store.Rule{}, fmt.Errorf("loading rule %d: %w", id, err)
	}
	return rule, nil
}

// Sites は保存済みの対象 site を返す。ルールが無い場合は空の一覧を返す。
func (s *Rules) Sites(ctx context.Context, id int64) ([]string, error) {
	return ruleSites(ctx, sqlcgen.New(s.pool), id)
}

// Create はルール本体と全ての子条件を原子的に作成する。
func (s *Rules) Create(ctx context.Context, in store.RuleInput) (store.Rule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.Rule{}, fmt.Errorf("beginning rule creation: %w", err)
	}
	defer rollback(tx)

	q := sqlcgen.New(tx)
	params, err := ruleCreateParams(in)
	if err != nil {
		return store.Rule{}, err
	}
	row, err := q.CreateRule(ctx, params)
	if err != nil {
		return store.Rule{}, fmt.Errorf("creating rule: %w", mapDatabaseError(err))
	}
	if err := replaceRuleChildren(ctx, q, row.ID, in); err != nil {
		return store.Rule{}, fmt.Errorf("replacing rule %d children: %w", row.ID, mapDatabaseError(err))
	}
	if err := s.bumpRulerInputRevisions(ctx, q, in.Sites, len(in.Sites) == 0); err != nil {
		return store.Rule{}, fmt.Errorf("bumping ruler input revisions after rule creation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Rule{}, fmt.Errorf("committing rule creation: %w", mapDatabaseError(err))
	}
	rule, err := loadRule(ctx, sqlcgen.New(s.pool), row)
	if err != nil {
		return store.Rule{}, fmt.Errorf("loading created rule %d: %w", row.ID, err)
	}
	return rule, nil
}

// Update はルール本体と全ての子条件を置き換える。同じルールへの更新は直列化する。
func (s *Rules) Update(ctx context.Context, id int64, in store.RuleInput) (store.Rule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.Rule{}, fmt.Errorf("beginning rule update: %w", err)
	}
	defer rollback(tx)

	params, err := ruleUpdateParams(id, in)
	if err != nil {
		return store.Rule{}, err
	}
	q := sqlcgen.New(tx)
	row, err := q.UpdateRule(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Rule{}, fmt.Errorf("rule %d: %w", id, store.ErrNotFound)
		}
		return store.Rule{}, fmt.Errorf("updating rule %d: %w", id, mapDatabaseError(err))
	}
	oldSites, err := ruleSites(ctx, q, id)
	if err != nil {
		return store.Rule{}, err
	}
	if err := replaceRuleChildren(ctx, q, row.ID, in); err != nil {
		return store.Rule{}, fmt.Errorf("replacing rule %d children: %w", id, mapDatabaseError(err))
	}
	sites := append(append([]string(nil), oldSites...), in.Sites...)
	global := len(oldSites) == 0 || len(in.Sites) == 0
	if err := s.bumpRulerInputRevisions(ctx, q, sites, global); err != nil {
		return store.Rule{}, fmt.Errorf("bumping ruler input revisions after rule update %d: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Rule{}, fmt.Errorf("committing rule update %d: %w", id, mapDatabaseError(err))
	}
	rule, err := loadRule(ctx, sqlcgen.New(s.pool), row)
	if err != nil {
		return store.Rule{}, fmt.Errorf("loading updated rule %d: %w", id, err)
	}
	return rule, nil
}

// Delete は、意図の無い予約の削除とルールの削除を原子的に行う。
func (s *Rules) Delete(ctx context.Context, id int64) (store.RuleDeleteResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.RuleDeleteResult{}, fmt.Errorf("beginning rule deletion: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if _, err := q.LockRuleForUpdate(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.RuleDeleteResult{}, fmt.Errorf("rule %d: %w", id, store.ErrNotFound)
		}
		return store.RuleDeleteResult{}, fmt.Errorf("locking rule %d before deletion: %w", id, err)
	}
	sites, err := ruleSites(ctx, q, id)
	if err != nil {
		return store.RuleDeleteResult{}, err
	}
	reservedID := id
	detached, err := q.CountReservationsByRuleWithIntent(ctx, &reservedID)
	if err != nil {
		return store.RuleDeleteResult{}, fmt.Errorf("counting reservations with intent for rule %d: %w", id, err)
	}
	deleted, err := q.DeleteReservationsByRuleWithoutIntent(ctx, &reservedID)
	if err != nil {
		return store.RuleDeleteResult{}, fmt.Errorf("deleting reservations without intent for rule %d: %w", id, err)
	}
	rows, err := q.DeleteRule(ctx, id)
	if err != nil {
		return store.RuleDeleteResult{}, fmt.Errorf("deleting rule %d: %w", id, mapDatabaseError(err))
	}
	if rows == 0 {
		return store.RuleDeleteResult{}, fmt.Errorf("rule %d: %w", id, store.ErrNotFound)
	}
	if err := s.bumpRulerInputRevisions(ctx, q, sites, len(sites) == 0); err != nil {
		return store.RuleDeleteResult{}, fmt.Errorf("bumping ruler input revisions after rule deletion %d: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.RuleDeleteResult{}, fmt.Errorf("committing rule deletion %d: %w", id, mapDatabaseError(err))
	}
	return store.RuleDeleteResult{DeletedReservations: deleted, DetachedReservations: detached, Sites: sites}, nil
}

// ImportEPGStationRule は EPGStation rule ID のルールを作成または更新し、子条件を全置換する。
func (s *Rules) ImportEPGStationRule(ctx context.Context, epgstationRuleID int64, in store.RuleInput) (bool, error) {
	metadata, err := json.Marshal(map[string]any{"epgstation": map[string]any{"ruleId": epgstationRuleID}})
	if err != nil {
		return false, fmt.Errorf("marshalling metadata for epgstation rule %d: %w", epgstationRuleID, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning epgstation rule import: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	existing, err := q.FindRuleByMetadata(ctx, metadata)
	var row sqlcgen.Rule
	created := false
	var oldSites []string
	switch {
	case err == nil:
		params, err := ruleUpdateParams(existing.ID, in)
		if err != nil {
			return false, err
		}
		params.Description = existing.Description
		params.Priority = existing.Priority
		params.DedupeEnabled = existing.DedupeEnabled
		params.DedupeThreshold = existing.DedupeThreshold
		params.DedupeWindow = existing.DedupeWindow
		params.KeepOriginal = existing.KeepOriginal
		params.EncodeProfiles = existing.EncodeProfiles
		params.Metadata = metadata
		row, err = q.UpdateRule(ctx, params)
		if err != nil {
			return false, fmt.Errorf("updating imported epgstation rule %d: %w", epgstationRuleID, mapDatabaseError(err))
		}
		oldSites, err = ruleSites(ctx, q, existing.ID)
		if err != nil {
			return false, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		created = true
		params, err := ruleCreateParams(in)
		if err != nil {
			return false, err
		}
		params.Metadata = metadata
		row, err = q.CreateRule(ctx, params)
		if err != nil {
			return false, fmt.Errorf("creating imported epgstation rule %d: %w", epgstationRuleID, mapDatabaseError(err))
		}
	default:
		return false, fmt.Errorf("looking up epgstation rule %d: %w", epgstationRuleID, err)
	}
	if err := replaceRuleChildren(ctx, q, row.ID, in); err != nil {
		return false, fmt.Errorf("writing imported epgstation rule %d children: %w", epgstationRuleID, mapDatabaseError(err))
	}
	sites := append(append([]string(nil), oldSites...), in.Sites...)
	global := len(in.Sites) == 0 || (!created && len(oldSites) == 0)
	if err := s.bumpRulerInputRevisions(ctx, q, sites, global); err != nil {
		return false, fmt.Errorf("bumping ruler input revisions after imported rule %d: %w", epgstationRuleID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing epgstation rule %d import: %w", epgstationRuleID, mapDatabaseError(err))
	}
	return created, nil
}

func loadRule(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.Rule) (store.Rule, error) {
	rule := ruleFromSQL(row)
	texts, err := q.ListRuleTextMatches(ctx, row.ID)
	if err != nil {
		return rule, fmt.Errorf("listing text matches: %w", err)
	}
	for _, text := range texts {
		rule.TextMatches = append(rule.TextMatches, store.RuleTextMatch{
			Target: text.Target, Mode: text.Mode, Value: text.Value,
			CaseSensitive: text.CaseSensitive, Negate: text.Negate,
		})
	}
	services, err := q.ListRuleServices(ctx, row.ID)
	if err != nil {
		return rule, fmt.Errorf("listing services: %w", err)
	}
	for _, service := range services {
		rule.Services = append(rule.Services, store.RuleService{NetworkID: service.NetworkID, ServiceID: service.ServiceID})
	}
	channelTypes, err := q.ListRuleChannelTypes(ctx, row.ID)
	if err != nil {
		return rule, fmt.Errorf("listing channel types: %w", err)
	}
	for _, channelType := range channelTypes {
		rule.ChannelTypes = append(rule.ChannelTypes, channelType.ChannelType)
	}
	genres, err := q.ListRuleGenres(ctx, row.ID)
	if err != nil {
		return rule, fmt.Errorf("listing genres: %w", err)
	}
	for _, genre := range genres {
		rule.Genres = append(rule.Genres, genre.GenreLv1)
	}
	times, err := q.ListRuleTimes(ctx, row.ID)
	if err != nil {
		return rule, fmt.Errorf("listing time windows: %w", err)
	}
	for _, tw := range times {
		rule.Times = append(rule.Times, store.RuleTimeWindow{Weekdays: tw.Weekdays, StartSec: tw.StartSec, EndSec: tw.EndSec})
	}
	rule.Sites, err = ruleSites(ctx, q, row.ID)
	if err != nil {
		return rule, err
	}
	return rule, nil
}

func ruleSites(ctx context.Context, q *sqlcgen.Queries, ruleID int64) ([]string, error) {
	rows, err := q.ListRuleSites(ctx, ruleID)
	if err != nil {
		return nil, fmt.Errorf("listing sites for rule %d: %w", ruleID, err)
	}
	sites := make([]string, 0, len(rows))
	for _, row := range rows {
		sites = append(sites, row.Site)
	}
	return sites, nil
}

func ruleCreateParams(in store.RuleInput) (sqlcgen.CreateRuleParams, error) {
	params := sqlcgen.CreateRuleParams{
		Name: in.Name, Description: "", Enabled: true, Priority: 10,
		KeepOriginal: "always", EncodeProfiles: []string{}, FilenameTemplate: "",
		Metadata: json.RawMessage("{}"),
	}
	if in.Description != nil {
		params.Description = *in.Description
	}
	if in.Enabled != nil {
		params.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		params.Priority = *in.Priority
	}
	if in.IsFree != nil {
		params.IsFree = pgtype.Bool{Bool: *in.IsFree, Valid: true}
	}
	params.DurationMinMs = in.DurationMinMs
	params.DurationMaxMs = in.DurationMaxMs
	params.PeriodStartAt = in.PeriodStartAt
	params.PeriodEndAt = in.PeriodEndAt
	if in.DedupeEnabled != nil {
		params.DedupeEnabled = *in.DedupeEnabled
	}
	if in.DedupeThreshold != nil {
		params.DedupeThreshold = pgtype.Float4{Float32: *in.DedupeThreshold, Valid: true}
	}
	if in.DedupeWindowSeconds != nil {
		params.DedupeWindow = secondsToInterval(*in.DedupeWindowSeconds)
	}
	if in.KeepOriginal != nil {
		params.KeepOriginal = *in.KeepOriginal
	}
	if in.EncodeProfiles != nil {
		params.EncodeProfiles = canonicalizeStrings(*in.EncodeProfiles)
	}
	if in.FilenameTemplate != nil {
		params.FilenameTemplate = *in.FilenameTemplate
	}
	if in.Metadata != nil {
		metadata, err := json.Marshal(*in.Metadata)
		if err != nil {
			return params, fmt.Errorf("marshalling rule metadata: %w", err)
		}
		params.Metadata = metadata
	}
	return params, nil
}

func ruleUpdateParams(id int64, in store.RuleInput) (sqlcgen.UpdateRuleParams, error) {
	created, err := ruleCreateParams(in)
	if err != nil {
		return sqlcgen.UpdateRuleParams{}, err
	}
	return sqlcgen.UpdateRuleParams{
		Name: created.Name, Description: created.Description, Enabled: created.Enabled, Priority: created.Priority,
		IsFree: created.IsFree, DurationMinMs: created.DurationMinMs, DurationMaxMs: created.DurationMaxMs,
		PeriodStartAt: created.PeriodStartAt, PeriodEndAt: created.PeriodEndAt,
		DedupeEnabled: created.DedupeEnabled, DedupeThreshold: created.DedupeThreshold, DedupeWindow: created.DedupeWindow,
		KeepOriginal: created.KeepOriginal, EncodeProfiles: created.EncodeProfiles,
		FilenameTemplate: created.FilenameTemplate, Metadata: created.Metadata, ID: id,
	}, nil
}

func replaceRuleChildren(ctx context.Context, q *sqlcgen.Queries, id int64, in store.RuleInput) error {
	if err := q.DeleteRuleTextMatches(ctx, id); err != nil {
		return fmt.Errorf("deleting text matches: %w", err)
	}
	if err := q.DeleteRuleServices(ctx, id); err != nil {
		return fmt.Errorf("deleting services: %w", err)
	}
	if err := q.DeleteRuleChannelTypes(ctx, id); err != nil {
		return fmt.Errorf("deleting channel types: %w", err)
	}
	if err := q.DeleteRuleGenres(ctx, id); err != nil {
		return fmt.Errorf("deleting genres: %w", err)
	}
	if err := q.DeleteRuleTimes(ctx, id); err != nil {
		return fmt.Errorf("deleting time windows: %w", err)
	}
	if err := q.DeleteRuleSites(ctx, id); err != nil {
		return fmt.Errorf("deleting sites: %w", err)
	}
	for i, text := range in.TextMatches {
		if err := q.InsertRuleTextMatch(ctx, sqlcgen.InsertRuleTextMatchParams{
			RuleID: id, Seq: int32(i), Target: text.Target, Mode: text.Mode, Value: text.Value,
			CaseSensitive: text.CaseSensitive, Negate: text.Negate,
		}); err != nil {
			return fmt.Errorf("inserting text match %d: %w", i, err)
		}
	}
	for _, service := range in.Services {
		if err := q.InsertRuleService(ctx, sqlcgen.InsertRuleServiceParams{
			RuleID: id, NetworkID: service.NetworkID, ServiceID: service.ServiceID,
		}); err != nil {
			return fmt.Errorf("inserting service: %w", err)
		}
	}
	for _, channelType := range in.ChannelTypes {
		if err := q.InsertRuleChannelType(ctx, sqlcgen.InsertRuleChannelTypeParams{RuleID: id, ChannelType: channelType}); err != nil {
			return fmt.Errorf("inserting channel type: %w", err)
		}
	}
	for _, genre := range in.Genres {
		if err := q.InsertRuleGenre(ctx, sqlcgen.InsertRuleGenreParams{RuleID: id, GenreLv1: genre}); err != nil {
			return fmt.Errorf("inserting genre: %w", err)
		}
	}
	for i, tw := range in.Times {
		if err := q.InsertRuleTime(ctx, sqlcgen.InsertRuleTimeParams{
			RuleID: id, Seq: int32(i), Weekdays: tw.Weekdays, StartSec: tw.StartSec, EndSec: tw.EndSec,
		}); err != nil {
			return fmt.Errorf("inserting time window %d: %w", i, err)
		}
	}
	for _, site := range in.Sites {
		if site == "" {
			return errors.New("rule site must not be empty")
		}
		if err := q.InsertRuleSite(ctx, sqlcgen.InsertRuleSiteParams{RuleID: id, Site: site}); err != nil {
			return fmt.Errorf("inserting site: %w", err)
		}
	}
	return nil
}

func ruleFromSQL(row sqlcgen.Rule) store.Rule {
	rule := store.Rule{
		ID: row.ID, Name: row.Name, Description: row.Description, Enabled: row.Enabled, Priority: row.Priority,
		DurationMinMs: row.DurationMinMs, DurationMaxMs: row.DurationMaxMs,
		PeriodStartAt: row.PeriodStartAt, PeriodEndAt: row.PeriodEndAt,
		DedupeEnabled: row.DedupeEnabled, KeepOriginal: row.KeepOriginal,
		EncodeProfiles: append([]string(nil), row.EncodeProfiles...), FilenameTemplate: row.FilenameTemplate,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.IsFree.Valid {
		value := row.IsFree.Bool
		rule.IsFree = &value
	}
	if row.DedupeThreshold.Valid {
		value := row.DedupeThreshold.Float32
		rule.DedupeThreshold = &value
	}
	if seconds, ok := intervalToSeconds(row.DedupeWindow); ok {
		rule.DedupeWindowSeconds = &seconds
	}
	if len(row.Metadata) > 0 && string(row.Metadata) != "{}" && string(row.Metadata) != "null" {
		var metadata map[string]any
		if err := json.Unmarshal(row.Metadata, &metadata); err == nil {
			rule.Metadata = metadata
		}
	}
	return rule
}

func secondsToInterval(seconds int64) pgtype.Interval {
	return pgtype.Interval{Microseconds: seconds * 1_000_000, Valid: true}
}

func intervalToSeconds(value pgtype.Interval) (int64, bool) {
	if !value.Valid {
		return 0, false
	}
	return value.Microseconds / 1_000_000, true
}

func canonicalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
