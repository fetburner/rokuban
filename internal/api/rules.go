package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/worker"
)

// ListRules はルール一覧を返す。
func (h *Server) ListRules(ctx context.Context, _ ListRulesRequestObject) (ListRulesResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		full, err := loadRule(ctx, q, row)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return ListRules200JSONResponse(out), nil
}

// GetRule は指定 ID のルールを返す。
func (h *Server) GetRule(ctx context.Context, req GetRuleRequestObject) (GetRuleResponseObject, error) {
	q := sqlcgen.New(h.pool)
	row, err := q.GetRule(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}
	full, err := loadRule(ctx, q, row)
	if err != nil {
		return nil, err
	}
	return GetRule200JSONResponse(full), nil
}

// CreateRule はルールを作成する。
func (h *Server) CreateRule(ctx context.Context, req CreateRuleRequestObject) (CreateRuleResponseObject, error) {
	if req.Body == nil {
		return CreateRule400JSONResponse{Error: "request body is required"}, nil
	}
	if err := validateRuleInput(ctx, h.pool, *req.Body); err != nil {
		return CreateRule400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	row, err := insertRule(ctx, q, *req.Body)
	if err != nil {
		return nil, err
	}
	if err := replaceRuleChildren(ctx, q, row.ID, *req.Body); err != nil {
		return nil, err
	}
	if err := h.insertRulerPassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	full, err := loadRule(ctx, sqlcgen.New(h.pool), row)
	if err != nil {
		return nil, err
	}
	return CreateRule201JSONResponse(full), nil
}

// UpdateRule はルールを上書き更新する（子テーブルは全置換）。
func (h *Server) UpdateRule(ctx context.Context, req UpdateRuleRequestObject) (UpdateRuleResponseObject, error) {
	if req.Body == nil {
		return UpdateRule400JSONResponse{Error: "request body is required"}, nil
	}
	if err := validateRuleInput(ctx, h.pool, *req.Body); err != nil {
		return UpdateRule400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.GetRule(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UpdateRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}

	params, err := ruleUpdateParams(req.Id, *req.Body)
	if err != nil {
		return UpdateRule400JSONResponse{Error: err.Error()}, nil
	}
	row, err := q.UpdateRule(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := replaceRuleChildren(ctx, q, row.ID, *req.Body); err != nil {
		return nil, err
	}
	if err := h.insertRulerPassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	full, err := loadRule(ctx, sqlcgen.New(h.pool), row)
	if err != nil {
		return nil, err
	}
	return UpdateRule200JSONResponse(full), nil
}

// DeleteRule はルールを削除する。overrides なし予約は削除、ありは detached 化。
func (h *Server) DeleteRule(ctx context.Context, req DeleteRuleRequestObject) (DeleteRuleResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.GetRule(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}

	// 内訳は先に数える（削除後には数えられない）。
	// 意図のある予約は残り、FK の ON DELETE SET NULL で rule_id が外れて
	// 実質 manual として動く。意図自体は program_intents にあるので消えない。
	id := req.Id
	detached, err := q.CountReservationsByRuleWithIntent(ctx, &id)
	if err != nil {
		return nil, fmt.Errorf("counting reservations with intent: %w", err)
	}
	deleted, err := q.DeleteReservationsByRuleWithoutIntent(ctx, &id)
	if err != nil {
		return nil, fmt.Errorf("deleting pure rule reservations: %w", err)
	}
	n, err := q.DeleteRule(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return DeleteRule404JSONResponse{Error: "rule not found"}, nil
	}
	if err := h.insertRulerPassHint(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return DeleteRule200JSONResponse{
		Id:                   req.Id,
		DeletedReservations:  int(deleted),
		DetachedReservations: int(detached),
	}, nil
}

// insertRulerPassHint はルール作成/更新/削除と同一トランザクションで RulerPassArgs を
// InsertTx する（ヒント経路。docs/recording.md §3.1「ルールの作成 / 更新 / 削除」）。
// dual-write を避けるため、ルール書き込みが失敗すればこのジョブも一緒にロールバックされる。
//
// h.river が nil の場合は何もしない（テストや、将来 River を持たない api 構成を許容する
// ため。RouterConfig.RiverClient のコメント参照）。RulerPassArgs.InsertOpts の
// UniqueOpts{ByArgs, ByState} により、同一サイトのヒントは定期実行に合流する。
func (h *Server) insertRulerPassHint(ctx context.Context, tx pgx.Tx) error {
	if h.river == nil {
		return nil
	}
	if _, err := h.river.InsertTx(ctx, tx, worker.RulerPassArgs{Site: defaultSite}, nil); err != nil {
		return fmt.Errorf("inserting ruler_pass hint: %w", err)
	}
	return nil
}

func validateRuleInput(ctx context.Context, pool *pgxpool.Pool, in RuleInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if in.KeepOriginal != nil && *in.KeepOriginal == RuleInputKeepOriginalUntilEncoded {
		if in.EncodeProfiles == nil || len(*in.EncodeProfiles) == 0 {
			return errors.New("encodeProfiles is required when keepOriginal is until_encoded")
		}
	}
	if in.DedupeEnabled != nil && *in.DedupeEnabled {
		if in.DedupeThreshold == nil {
			return errors.New("dedupeThreshold is required when dedupeEnabled is true")
		}
	}
	// dedupeThreshold は pg_trgm の similarity() と比較される（internal/ruler/dedupe.go の
	// similarity(rec.title, c.title) >= c.dedupe_threshold）。similarity() の値域は (0, 1] の
	// 半開区間として扱う: 0 を許すと "similarity() >= 0" が恒真になり、そのルールに finished の
	// 録画が 1 本でもあれば以降マッチする全番組に base.skip が立って録画が黙って止まる
	// （サーキットブレーカーは削除しか守らないのでこの経路は何にも止められない）。
	// 1 を超えると恒偽になり重複排除が黙って無効化される。したがって 0 は許さず、
	// (0, 1] の範囲だけを許可する。
	if in.DedupeThreshold != nil {
		if v := *in.DedupeThreshold; v <= 0 || v > 1 {
			return fmt.Errorf("dedupeThreshold must be > 0 and <= 1 (similarity() ranges over [0,1]; 0 would match every program and silently stop recording), got %v", v)
		}
	}
	// dedupeWindowSeconds は interval に変換され、
	// "rec.program_start_at >= now() - c.dedupe_window" の右辺に使われる。負値を許すと
	// 右辺が未来の時刻になり条件が恒偽になる（重複排除が黙って無効化される）ため負値を弾く。
	// 0 は「時間窓なし相当」として許容する（危険は恒偽側だけなので片側のみ弾けば足りる）。
	if in.DedupeWindowSeconds != nil && *in.DedupeWindowSeconds < 0 {
		return fmt.Errorf("dedupeWindowSeconds must not be negative, got %d", *in.DedupeWindowSeconds)
	}
	if in.TextMatches != nil {
		q := sqlcgen.New(pool)
		for _, m := range *in.TextMatches {
			if m.Mode == Regex && m.Value != "" {
				if err := q.ValidateRegexPattern(ctx, m.Value); err != nil {
					return fmt.Errorf("invalid regex %q (POSIX ARE; lookbehind is not supported): %w", m.Value, err)
				}
			}
		}
	}
	if in.FilenameTemplate != nil && *in.FilenameTemplate != "" {
		if err := contentpath.Validate(*in.FilenameTemplate); err != nil {
			return fmt.Errorf("invalid filenameTemplate %q (Go text/template; see docs/recording.md §3.2): %w",
				*in.FilenameTemplate, err)
		}
	}
	return nil
}

func insertRule(ctx context.Context, q *sqlcgen.Queries, in RuleInput) (sqlcgen.Rule, error) {
	params, err := ruleCreateParams(in)
	if err != nil {
		return sqlcgen.Rule{}, err
	}
	return q.CreateRule(ctx, params)
}

func ruleCreateParams(in RuleInput) (sqlcgen.CreateRuleParams, error) {
	p := sqlcgen.CreateRuleParams{
		Name:             strings.TrimSpace(in.Name),
		Description:      derefStr(in.Description),
		Enabled:          true,
		Priority:         10,
		KeepOriginal:     "always",
		EncodeProfiles:   []string{},
		FilenameTemplate: "",
		Metadata:         json.RawMessage("{}"),
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		p.Priority = int32(*in.Priority)
	}
	if in.IsFree != nil {
		p.IsFree = pgtype.Bool{Bool: *in.IsFree, Valid: true}
	}
	p.DurationMinMs = in.DurationMinMs
	p.DurationMaxMs = in.DurationMaxMs
	if in.PeriodStartAt != nil {
		t := *in.PeriodStartAt
		p.PeriodStartAt = &t
	}
	if in.PeriodEndAt != nil {
		t := *in.PeriodEndAt
		p.PeriodEndAt = &t
	}
	if in.DedupeEnabled != nil {
		p.DedupeEnabled = *in.DedupeEnabled
	}
	if in.DedupeThreshold != nil {
		p.DedupeThreshold = pgtype.Float4{Float32: *in.DedupeThreshold, Valid: true}
	}
	if in.DedupeWindowSeconds != nil {
		p.DedupeWindow = secondsToInterval(*in.DedupeWindowSeconds)
	}
	if in.KeepOriginal != nil {
		p.KeepOriginal = string(*in.KeepOriginal)
	}
	if in.EncodeProfiles != nil {
		p.EncodeProfiles = canonicalizeStrings(*in.EncodeProfiles)
	}
	if in.FilenameTemplate != nil {
		p.FilenameTemplate = *in.FilenameTemplate
	}
	if in.Metadata != nil {
		b, err := json.Marshal(in.Metadata)
		if err != nil {
			return p, fmt.Errorf("marshalling metadata: %w", err)
		}
		p.Metadata = b
	}
	return p, nil
}

func ruleUpdateParams(id int64, in RuleInput) (sqlcgen.UpdateRuleParams, error) {
	c, err := ruleCreateParams(in)
	if err != nil {
		return sqlcgen.UpdateRuleParams{}, err
	}
	return sqlcgen.UpdateRuleParams{
		Name:             c.Name,
		Description:      c.Description,
		Enabled:          c.Enabled,
		Priority:         c.Priority,
		IsFree:           c.IsFree,
		DurationMinMs:    c.DurationMinMs,
		DurationMaxMs:    c.DurationMaxMs,
		PeriodStartAt:    c.PeriodStartAt,
		PeriodEndAt:      c.PeriodEndAt,
		DedupeEnabled:    c.DedupeEnabled,
		DedupeThreshold:  c.DedupeThreshold,
		DedupeWindow:     c.DedupeWindow,
		KeepOriginal:     c.KeepOriginal,
		EncodeProfiles:   c.EncodeProfiles,
		FilenameTemplate: c.FilenameTemplate,
		Metadata:         c.Metadata,
		ID:               id,
	}, nil
}

func replaceRuleChildren(ctx context.Context, q *sqlcgen.Queries, ruleID int64, in RuleInput) error {
	if err := q.DeleteRuleTextMatches(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleServices(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleChannelTypes(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleGenres(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleTimes(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleSites(ctx, ruleID); err != nil {
		return err
	}

	if in.TextMatches != nil {
		for i, m := range *in.TextMatches {
			if err := q.InsertRuleTextMatch(ctx, sqlcgen.InsertRuleTextMatchParams{
				RuleID:        ruleID,
				Seq:           int32(i),
				Target:        string(m.Target),
				Mode:          string(m.Mode),
				Value:         m.Value,
				CaseSensitive: derefBool(m.CaseSensitive),
				Negate:        derefBool(m.Negate),
			}); err != nil {
				return fmt.Errorf("inserting text match %d: %w", i, err)
			}
		}
	}
	if in.Services != nil {
		for _, s := range *in.Services {
			if err := q.InsertRuleService(ctx, sqlcgen.InsertRuleServiceParams{
				RuleID:    ruleID,
				NetworkID: int32(s.NetworkId),
				ServiceID: int32(s.ServiceId),
			}); err != nil {
				return fmt.Errorf("inserting service: %w", err)
			}
		}
	}
	if in.ChannelTypes != nil {
		for _, ct := range *in.ChannelTypes {
			if err := q.InsertRuleChannelType(ctx, sqlcgen.InsertRuleChannelTypeParams{
				RuleID:      ruleID,
				ChannelType: string(ct),
			}); err != nil {
				return fmt.Errorf("inserting channel type: %w", err)
			}
		}
	}
	if in.Genres != nil {
		for _, g := range *in.Genres {
			if err := q.InsertRuleGenre(ctx, sqlcgen.InsertRuleGenreParams{
				RuleID:   ruleID,
				GenreLv1: int16(g),
			}); err != nil {
				return fmt.Errorf("inserting genre: %w", err)
			}
		}
	}
	if in.Times != nil {
		for i, tw := range *in.Times {
			if err := q.InsertRuleTime(ctx, sqlcgen.InsertRuleTimeParams{
				RuleID:   ruleID,
				Seq:      int32(i),
				Weekdays: int32(tw.Weekdays),
				StartSec: int32(tw.StartSec),
				EndSec:   int32(tw.EndSec),
			}); err != nil {
				return fmt.Errorf("inserting time window %d: %w", i, err)
			}
		}
	}
	if in.Sites != nil {
		for _, site := range *in.Sites {
			if site == "" {
				continue
			}
			if err := q.InsertRuleSite(ctx, sqlcgen.InsertRuleSiteParams{
				RuleID: ruleID,
				Site:   site,
			}); err != nil {
				return fmt.Errorf("inserting site: %w", err)
			}
		}
	}
	return nil
}

func loadRule(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.Rule) (Rule, error) {
	r := ruleFromRow(row)

	texts, err := q.ListRuleTextMatches(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(texts) > 0 {
		ms := make([]RuleTextMatch, 0, len(texts))
		for _, t := range texts {
			cs, neg := t.CaseSensitive, t.Negate
			ms = append(ms, RuleTextMatch{
				Target:        RuleTextMatchTarget(t.Target),
				Mode:          RuleTextMatchMode(t.Mode),
				Value:         t.Value,
				CaseSensitive: &cs,
				Negate:        &neg,
			})
		}
		r.TextMatches = &ms
	}

	services, err := q.ListRuleServices(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(services) > 0 {
		ss := make([]RuleService, 0, len(services))
		for _, s := range services {
			ss = append(ss, RuleService{NetworkId: int(s.NetworkID), ServiceId: int(s.ServiceID)})
		}
		r.Services = &ss
	}

	cts, err := q.ListRuleChannelTypes(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(cts) > 0 {
		out := make([]RuleChannelTypes, 0, len(cts))
		for _, c := range cts {
			out = append(out, RuleChannelTypes(c.ChannelType))
		}
		r.ChannelTypes = &out
	}

	genres, err := q.ListRuleGenres(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(genres) > 0 {
		gs := make([]int, 0, len(genres))
		for _, g := range genres {
			gs = append(gs, int(g.GenreLv1))
		}
		r.Genres = &gs
	}

	times, err := q.ListRuleTimes(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(times) > 0 {
		ts := make([]RuleTimeWindow, 0, len(times))
		for _, t := range times {
			ts = append(ts, RuleTimeWindow{
				Weekdays: int(t.Weekdays),
				StartSec: int(t.StartSec),
				EndSec:   int(t.EndSec),
			})
		}
		r.Times = &ts
	}

	sites, err := q.ListRuleSites(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(sites) > 0 {
		ss := make([]string, 0, len(sites))
		for _, s := range sites {
			ss = append(ss, s.Site)
		}
		r.Sites = &ss
	}

	return r, nil
}

func ruleFromRow(row sqlcgen.Rule) Rule {
	desc := row.Description
	ft := row.FilenameTemplate
	r := Rule{
		Id:               row.ID,
		Name:             row.Name,
		Description:      &desc,
		Enabled:          row.Enabled,
		Priority:         int(row.Priority),
		KeepOriginal:     RuleKeepOriginal(row.KeepOriginal),
		FilenameTemplate: &ft,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
	if row.IsFree.Valid {
		v := row.IsFree.Bool
		r.IsFree = &v
	}
	r.DurationMinMs = row.DurationMinMs
	r.DurationMaxMs = row.DurationMaxMs
	r.PeriodStartAt = row.PeriodStartAt
	r.PeriodEndAt = row.PeriodEndAt
	de := row.DedupeEnabled
	r.DedupeEnabled = &de
	if row.DedupeThreshold.Valid {
		v := row.DedupeThreshold.Float32
		r.DedupeThreshold = &v
	}
	if secs, ok := intervalToSeconds(row.DedupeWindow); ok {
		r.DedupeWindowSeconds = &secs
	}
	if len(row.EncodeProfiles) > 0 {
		ep := append([]string(nil), row.EncodeProfiles...)
		r.EncodeProfiles = &ep
	}
	if len(row.Metadata) > 0 && string(row.Metadata) != "{}" && string(row.Metadata) != "null" {
		var m map[string]interface{}
		if json.Unmarshal(row.Metadata, &m) == nil {
			r.Metadata = &m
		}
	}
	return r
}

func secondsToInterval(sec int64) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: sec * 1_000_000,
		Valid:        true,
	}
}

func intervalToSeconds(iv pgtype.Interval) (int64, bool) {
	if !iv.Valid {
		return 0, false
	}
	return iv.Microseconds / 1_000_000, true
}

func canonicalizeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}
