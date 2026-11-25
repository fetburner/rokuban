package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// Rules は MySQL で録画ルールの読み書きと EPGStation からの取り込みを実装する。
// 書き込みは runTx で 1 メソッド = 1 tx にし、子表は親の更新と同じ tx で全置換する。
type Rules struct {
	db        *sql.DB
	siteNames []string
}

var _ store.Rules = (*Rules)(nil)
var _ store.RuleImport = (*Rules)(nil)

// NewRules は MySQL を使う Rules を作る。siteNames は構成の全 site で、
// sites が空のルールの書き込みがこの全 site の版を進める。空の一覧は
// 呼び出し側の配線の誤りなので panic する（黙って 1 site を仮定すると、
// 他の site の版が進まず古い評価がコミットされる）。
func NewRules(db *sql.DB, siteNames []string) *Rules {
	if len(siteNames) == 0 {
		panic("mysqlstore.NewRules: siteNames must not be empty")
	}
	return &Rules{db: db, siteNames: append([]string(nil), siteNames...)}
}

const ruleColumns = `id, name, description, enabled, priority, is_free, duration_min_ms, duration_max_ms,
 period_start_at, period_end_at, dedupe_enabled, dedupe_threshold, dedupe_window,
 keep_original, encode_profiles, filename_template, metadata, created_at, updated_at`

const ruleInsertSQL = `
 INSERT INTO rules (name, description, enabled, priority, is_free, duration_min_ms, duration_max_ms,
   period_start_at, period_end_at, dedupe_enabled, dedupe_threshold, dedupe_window,
   keep_original, encode_profiles, filename_template, metadata)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// List は子条件を含むルールを優先度降順、ID 昇順で返す。
func (s *Rules) List(ctx context.Context) ([]store.Rule, error) {
	return listRules(ctx, s.db, "")
}

// Get は子条件を含むルールを返す。存在しないときは ErrNotFound を返す。
func (s *Rules) Get(ctx context.Context, id int64) (store.Rule, error) {
	return getRule(ctx, s.db, id)
}

// Sites は保存済みの対象 site を返す。ルールが無い場合は空の一覧を返す。
func (s *Rules) Sites(ctx context.Context, id int64) ([]string, error) {
	return ruleSites(ctx, s.db, id)
}

// Create はルール本体と全ての子条件を原子的に作成する。
func (s *Rules) Create(ctx context.Context, in store.RuleInput) (store.Rule, error) {
	values, err := newRuleValues(in)
	if err != nil {
		return store.Rule{}, err
	}
	var created store.Rule
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, ruleInsertSQL, values.insertArgs()...)
		if err != nil {
			return fmt.Errorf("creating rule: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading created rule ID: %w", err)
		}
		if err := replaceRuleChildren(ctx, tx, id, in); err != nil {
			return fmt.Errorf("replacing rule %d children: %w", id, err)
		}
		if err := s.bumpRulerInputRevisions(ctx, tx, in.Sites, len(in.Sites) == 0); err != nil {
			return fmt.Errorf("bumping ruler input revisions after rule creation: %w", err)
		}
		created, err = getRule(ctx, tx, id)
		return err
	})
	if err != nil {
		return store.Rule{}, err
	}
	return created, nil
}

// Update はルール本体と全ての子条件を置き換える。同じルールへの更新は直列化する。
func (s *Rules) Update(ctx context.Context, id int64, in store.RuleInput) (store.Rule, error) {
	values, err := newRuleValues(in)
	if err != nil {
		return store.Rule{}, err
	}
	var updated store.Rule
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockRule(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
 UPDATE rules SET name = ?, description = ?, enabled = ?, priority = ?, is_free = ?,
   duration_min_ms = ?, duration_max_ms = ?, period_start_at = ?, period_end_at = ?,
   dedupe_enabled = ?, dedupe_threshold = ?, dedupe_window = ?, keep_original = ?,
   encode_profiles = ?, filename_template = ?, metadata = ?, updated_at = CURRENT_TIMESTAMP(6)
 WHERE id = ?`, append(values.insertArgs(), id)...); err != nil {
			return fmt.Errorf("updating rule %d: %w", id, err)
		}
		oldSites, err := ruleSites(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := replaceRuleChildren(ctx, tx, id, in); err != nil {
			return fmt.Errorf("replacing rule %d children: %w", id, err)
		}
		sites := append(slices.Clone(oldSites), in.Sites...)
		global := len(oldSites) == 0 || len(in.Sites) == 0
		if err := s.bumpRulerInputRevisions(ctx, tx, sites, global); err != nil {
			return fmt.Errorf("bumping ruler input revisions after rule update %d: %w", id, err)
		}
		updated, err = getRule(ctx, tx, id)
		return err
	})
	if err != nil {
		return store.Rule{}, err
	}
	return updated, nil
}

// Delete は、意図の無い予約の削除とルールの削除を原子的に行う。
// 意図のある予約は残し、ルール参照だけ外す。
func (s *Rules) Delete(ctx context.Context, id int64) (store.RuleDeleteResult, error) {
	var result store.RuleDeleteResult
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockRule(ctx, tx, id); err != nil {
			return err
		}
		sites, err := ruleSites(ctx, tx, id)
		if err != nil {
			return err
		}
		var detached int64
		if err := tx.QueryRowContext(ctx, `
 SELECT COUNT(*) FROM reservations r
 WHERE r.rule_id = ? AND EXISTS (
   SELECT 1 FROM program_investments v WHERE v.site = r.site AND v.program_id = r.program_id)
 `, id).Scan(&detached); err != nil {
			return fmt.Errorf("counting reservations with intent for rule %d: %w", id, err)
		}
		// 明示の DELETE / UPDATE は通知トリガーを発火させる。FK の ON DELETE は発火させないので使わない。
		res, err := tx.ExecContext(ctx, `
 DELETE FROM reservations
 WHERE rule_id = ? AND NOT EXISTS (
   SELECT 1 FROM program_investments v WHERE v.site = reservations.site AND v.program_id = reservations.program_id)
 `, id)
		if err != nil {
			return fmt.Errorf("deleting reservations without intent for rule %d: %w", id, err)
		}
		deleted, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting deleted reservations for rule %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reservations SET rule_id = NULL WHERE rule_id = ?`, id); err != nil {
			return fmt.Errorf("detaching reservations from rule %d: %w", id, err)
		}
		// 録画の参照も明示の UPDATE で外す。FK の ON DELETE SET NULL は通知トリガーを発火させない。
		if _, err := tx.ExecContext(ctx, `UPDATE recordings SET rule_id = NULL WHERE rule_id = ?`, id); err != nil {
			return fmt.Errorf("detaching recordings from rule %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id); err != nil {
			return fmt.Errorf("deleting rule %d: %w", id, err)
		}
		if err := s.bumpRulerInputRevisions(ctx, tx, sites, len(sites) == 0); err != nil {
			return fmt.Errorf("bumping ruler input revisions after rule deletion %d: %w", id, err)
		}
		result = store.RuleDeleteResult{DeletedReservations: deleted, DetachedReservations: detached, Sites: sites}
		return nil
	})
	if err != nil {
		return store.RuleDeleteResult{}, err
	}
	return result, nil
}

// ImportEPGStationRule は EPGStation rule ID のルールを作成または更新し、子条件を全置換する。
// 更新では取り込みが書く列だけを UPDATE し、説明・優先度・重複判定・保持・メタデータは
// 既存行の値を残す（同時に書かれた手動の値を取り込みが上書きしない）。
// 同じ新しい ID への同時取り込みは一意キーで 1 本だけが作成し、負けた側は ErrConflict になる。
func (s *Rules) ImportEPGStationRule(ctx context.Context, epgstationRuleID int64, in store.RuleInput) (bool, error) {
	externalID := strconv.FormatInt(epgstationRuleID, 10)
	values, err := newRuleValues(in)
	if err != nil {
		return false, err
	}
	created := false
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		var id int64
		var oldSites []string
		err := tx.QueryRowContext(ctx, `
 SELECT rule_id FROM rule_import_keys WHERE source = 'epgstation' AND external_id = ?
 `, externalID).Scan(&id)
		switch {
		case err == nil:
			if err := lockRule(ctx, tx, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
 UPDATE rules SET name = ?, enabled = ?, is_free = ?, duration_min_ms = ?, duration_max_ms = ?,
   period_start_at = ?, period_end_at = ?, filename_template = ?, updated_at = CURRENT_TIMESTAMP(6)
 WHERE id = ?`, append(values.importUpdateArgs(), id)...); err != nil {
				return fmt.Errorf("updating imported epgstation rule %d: %w", epgstationRuleID, err)
			}
			if oldSites, err = ruleSites(ctx, tx, id); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			created = true
			res, err := tx.ExecContext(ctx, ruleInsertSQL, values.insertArgs()...)
			if err != nil {
				return fmt.Errorf("creating imported epgstation rule %d: %w", epgstationRuleID, err)
			}
			if id, err = res.LastInsertId(); err != nil {
				return fmt.Errorf("reading created rule ID: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_import_keys (rule_id, source, external_id) VALUES (?, 'epgstation', ?)
 `, id, externalID); err != nil {
				return fmt.Errorf("creating rule import key: %w", err)
			}
		default:
			return fmt.Errorf("looking up epgstation rule %d: %w", epgstationRuleID, err)
		}
		if err := replaceRuleChildren(ctx, tx, id, in); err != nil {
			return fmt.Errorf("writing imported epgstation rule %d children: %w", epgstationRuleID, err)
		}
		sites := append(slices.Clone(oldSites), in.Sites...)
		global := len(in.Sites) == 0 || (!created && len(oldSites) == 0)
		if err := s.bumpRulerInputRevisions(ctx, tx, sites, global); err != nil {
			return fmt.Errorf("bumping ruler input revisions after imported rule %d: %w", epgstationRuleID, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// bumpRulerInputRevisions は書いた tx の中で対象 site の版を 1 つずつ進める。
// all が true なら構成の全 site を対象にする。site は昇順に進めて、並行する書き込みの
// ロック取得順を揃える。
func (s *Rules) bumpRulerInputRevisions(ctx context.Context, tx *sql.Tx, sites []string, all bool) error {
	if all {
		sites = s.siteNames
	}
	sorted := slices.Clone(sites)
	slices.Sort(sorted)
	for _, site := range slices.Compact(sorted) {
		if err := bumpRulerInputRevision(ctx, tx, site); err != nil {
			return err
		}
	}
	return nil
}

// lockRule はルール行を FOR UPDATE で取り、同じルールへの書き込みを直列化する。存在しなければ ErrNotFound を返す。
func lockRule(ctx context.Context, tx *sql.Tx, id int64) error {
	var locked int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM rules WHERE id = ? FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rule %d: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("locking rule %d: %w", id, err)
	}
	return nil
}

// getRule は子条件を含むルール 1 件を読む。tx を渡せば、その tx の書き込みが見える。
func getRule(ctx context.Context, db rowQuerier, id int64) (store.Rule, error) {
	rules, err := listRules(ctx, db, "WHERE id = ?", id)
	if err != nil {
		return store.Rule{}, fmt.Errorf("getting rule %d: %w", id, err)
	}
	if len(rules) == 0 {
		return store.Rule{}, fmt.Errorf("rule %d: %w", id, store.ErrNotFound)
	}
	return rules[0], nil
}

// listRules は where（省略可）で絞ったルールを子条件つきで、優先度降順・ID 昇順に返す。
func listRules(ctx context.Context, db rowQuerier, where string, args ...any) ([]store.Rule, error) {
	rules, err := queryRows(ctx, db, "rules", "SELECT "+ruleColumns+" FROM rules "+where+" ORDER BY priority DESC, id ASC",
		scanRule, args...)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		if err := loadRuleChildren(ctx, db, &rules[i]); err != nil {
			return nil, fmt.Errorf("loading rule %d: %w", rules[i].ID, err)
		}
	}
	return rules, nil
}

func scanRule(rows *sql.Rows) (store.Rule, error) {
	var r store.Rule
	var profiles, metadata []byte
	if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.Enabled, &r.Priority, &r.IsFree, &r.DurationMinMs,
		&r.DurationMaxMs, &r.PeriodStartAt, &r.PeriodEndAt, &r.DedupeEnabled, &r.DedupeThreshold, &r.DedupeWindowSeconds,
		&r.KeepOriginal, &profiles, &r.FilenameTemplate, &metadata, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	if err := json.Unmarshal(profiles, &r.EncodeProfiles); err != nil {
		return r, fmt.Errorf("decoding encode profiles: %w", err)
	}
	if err := json.Unmarshal(metadata, &r.Metadata); err != nil {
		return r, fmt.Errorf("decoding rule metadata: %w", err)
	}
	if len(r.Metadata) == 0 {
		r.Metadata = nil
	}
	if len(r.EncodeProfiles) == 0 {
		r.EncodeProfiles = nil
	}
	return r, nil
}

func loadRuleChildren(ctx context.Context, db rowQuerier, r *store.Rule) error {
	var err error
	if r.TextMatches, err = queryRows(ctx, db, "text matches", `
 SELECT target, mode, value, case_sensitive, negate FROM rule_text_matches WHERE rule_id = ? ORDER BY seq`,
		func(rows *sql.Rows) (store.RuleTextMatch, error) {
			var m store.RuleTextMatch
			err := rows.Scan(&m.Target, &m.Mode, &m.Value, &m.CaseSensitive, &m.Negate)
			return m, err
		}, r.ID); err != nil {
		return err
	}
	if r.Services, err = queryRows(ctx, db, "services", `
 SELECT network_id, service_id FROM rule_services WHERE rule_id = ? ORDER BY network_id, service_id`,
		func(rows *sql.Rows) (store.RuleService, error) {
			var s store.RuleService
			err := rows.Scan(&s.NetworkID, &s.ServiceID)
			return s, err
		}, r.ID); err != nil {
		return err
	}
	if r.ChannelTypes, err = queryRows(ctx, db, "channel types", `
 SELECT channel_type FROM rule_channel_types WHERE rule_id = ? ORDER BY channel_type`,
		scanString, r.ID); err != nil {
		return err
	}
	if r.Genres, err = queryRows(ctx, db, "genres", `
 SELECT genre_lv1 FROM rule_genres WHERE rule_id = ? ORDER BY genre_lv1`,
		func(rows *sql.Rows) (int16, error) {
			var genre int16
			err := rows.Scan(&genre)
			return genre, err
		}, r.ID); err != nil {
		return err
	}
	if r.Times, err = queryRows(ctx, db, "time windows", `
 SELECT weekdays, start_sec, end_sec FROM rule_times WHERE rule_id = ? ORDER BY seq`,
		func(rows *sql.Rows) (store.RuleTimeWindow, error) {
			var w store.RuleTimeWindow
			err := rows.Scan(&w.Weekdays, &w.StartSec, &w.EndSec)
			return w, err
		}, r.ID); err != nil {
		return err
	}
	r.Sites, err = ruleSites(ctx, db, r.ID)
	return err
}

func ruleSites(ctx context.Context, db rowQuerier, id int64) ([]string, error) {
	return queryRows(ctx, db, "rule sites", `SELECT site FROM rule_sites WHERE rule_id = ? ORDER BY site`, scanString, id)
}

func scanString(rows *sql.Rows) (string, error) {
	var s string
	err := rows.Scan(&s)
	return s, err
}

// replaceRuleChildren は子表を全て消して入れ直す。呼び出し側の tx が親の更新と一緒に確定する。
func replaceRuleChildren(ctx context.Context, tx *sql.Tx, id int64, in store.RuleInput) error {
	for _, table := range []string{"rule_text_matches", "rule_services", "rule_channel_types", "rule_genres", "rule_times", "rule_sites"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rule_id = ?", id); err != nil {
			return fmt.Errorf("deleting %s: %w", table, err)
		}
	}
	for i, text := range in.TextMatches {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, i, text.Target, text.Mode, text.Value, text.CaseSensitive, text.Negate); err != nil {
			return fmt.Errorf("inserting text match %d: %w", i, err)
		}
	}
	for _, service := range in.Services {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rule_services (rule_id, network_id, service_id) VALUES (?, ?, ?)`,
			id, service.NetworkID, service.ServiceID); err != nil {
			return fmt.Errorf("inserting service: %w", err)
		}
	}
	for _, channelType := range in.ChannelTypes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rule_channel_types (rule_id, channel_type) VALUES (?, ?)`, id, channelType); err != nil {
			return fmt.Errorf("inserting channel type: %w", err)
		}
	}
	for _, genre := range in.Genres {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rule_genres (rule_id, genre_lv1) VALUES (?, ?)`, id, genre); err != nil {
			return fmt.Errorf("inserting genre: %w", err)
		}
	}
	for i, window := range in.Times {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_times (rule_id, seq, weekdays, start_sec, end_sec) VALUES (?, ?, ?, ?, ?)`,
			id, i, window.Weekdays, window.StartSec, window.EndSec); err != nil {
			return fmt.Errorf("inserting time window %d: %w", i, err)
		}
	}
	for _, site := range in.Sites {
		if site == "" {
			return errors.New("rule site must not be empty")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rule_sites (rule_id, site) VALUES (?, ?)`, id, site); err != nil {
			return fmt.Errorf("inserting site: %w", err)
		}
	}
	return nil
}

// ruleValues は rules 表へ書く値である。未指定は既定値に直す（Postgres の ruleCreateParams と同じ既定）。
type ruleValues struct {
	name                string
	description         string
	enabled             bool
	priority            int32
	isFree              *bool
	durationMinMs       *int64
	durationMaxMs       *int64
	periodStartAt       *time.Time
	periodEndAt         *time.Time
	dedupeEnabled       bool
	dedupeThreshold     *float32
	dedupeWindowSeconds *int64
	keepOriginal        string
	encodeProfiles      string
	filenameTemplate    string
	metadata            string
}

func newRuleValues(in store.RuleInput) (ruleValues, error) {
	v := ruleValues{
		name: in.Name, enabled: true, priority: 10, keepOriginal: "always",
		encodeProfiles: "[]", metadata: "{}",
		isFree: in.IsFree, durationMinMs: in.DurationMinMs, durationMaxMs: in.DurationMaxMs,
		periodStartAt: in.PeriodStartAt, periodEndAt: in.PeriodEndAt,
		dedupeThreshold: in.DedupeThreshold, dedupeWindowSeconds: in.DedupeWindowSeconds,
	}
	if in.Description != nil {
		v.description = *in.Description
	}
	if in.Enabled != nil {
		v.enabled = *in.Enabled
	}
	if in.Priority != nil {
		v.priority = *in.Priority
	}
	if in.DedupeEnabled != nil {
		v.dedupeEnabled = *in.DedupeEnabled
	}
	if in.KeepOriginal != nil {
		v.keepOriginal = *in.KeepOriginal
	}
	if in.EncodeProfiles != nil {
		profiles, err := json.Marshal(store.CanonicalizeStrings(*in.EncodeProfiles))
		if err != nil {
			return v, fmt.Errorf("encoding encode profiles: %w", err)
		}
		v.encodeProfiles = string(profiles)
	}
	if in.FilenameTemplate != nil {
		v.filenameTemplate = *in.FilenameTemplate
	}
	if in.Metadata != nil {
		metadata := *in.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		raw, err := json.Marshal(metadata)
		if err != nil {
			return v, fmt.Errorf("marshalling rule metadata: %w", err)
		}
		v.metadata = string(raw)
	}
	return v, nil
}

// insertArgs は ruleInsertSQL の列順（id と日時を除く）に値を並べる。全列の UPDATE の SET 順もこれと同じである。
func (v ruleValues) insertArgs() []any {
	return []any{v.name, v.description, v.enabled, v.priority, v.isFree, v.durationMinMs, v.durationMaxMs,
		v.periodStartAt, v.periodEndAt, v.dedupeEnabled, v.dedupeThreshold, v.dedupeWindowSeconds,
		v.keepOriginal, v.encodeProfiles, v.filenameTemplate, v.metadata}
}

// importUpdateArgs は取り込みの UPDATE の SET 順に値を並べる。説明・優先度・重複判定・保持・メタデータは取り込みが書かない。
func (v ruleValues) importUpdateArgs() []any {
	return []any{v.name, v.enabled, v.isFree, v.durationMinMs, v.durationMaxMs,
		v.periodStartAt, v.periodEndAt, v.filenameTemplate}
}
