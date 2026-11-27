package mysqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/serieskey"
	"github.com/fetburner/rokuban/internal/store"
)

// Restore は catalog の全表を 1 tx で書き、分類ルールを再評価する。
// 順序: 再評価の名前付きロック → rules（+ 子）→ rule_import_keys → label_rules →
// program_snapshots → program_intents / program_overrides → recordings → recording_purge_requests →
// recording_watched → recording_processing_policy → media_assets と物理コピー → チャプター所有 2 表 →
// drop_stats → drop_positions → media_asset_ts_scans → 分類ルールの全件再評価 → ruler 入力の版の更新。
// 全部 1 トランザクションで、途中失敗なら何も残さない。
//
// ID の採番は、明示 ID の挿入で AUTO_INCREMENT が最大値 + 1 以上に進むことに任せる（検査: TestCatalogRestoreAdvancesAutoIncrement）。
// Postgres 版のように下げることはしない（MySQL で下げるには ALTER TABLE が要り、tx を暗黙に commit する）。
// 下がらないので欠番は残るが、復元後の挿入が既存の ID と衝突することは無い。
func (s *Catalog) Restore(ctx context.Context, doc *catalogdoc.Document, options store.RestoreOptions) (*store.RestoreResult, error) {
	if doc == nil {
		return nil, fmt.Errorf("catalog document is nil")
	}
	normalized, err := catalogdoc.NormalizeRuleImportKeys(doc)
	if err != nil {
		return nil, fmt.Errorf("normalizing rule import keys: %w", err)
	}
	var result *store.RestoreResult
	err = runTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := applyDocument(ctx, tx, normalized, options, s.siteNames)
		if err != nil {
			return err
		}
		result = res
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("restoring catalog: %w", err)
	}
	return result, nil
}

// snapshotKey は program_snapshots の主キー。
type snapshotKey struct {
	site      string
	programID int64
}

// insertableSnapshot は snapshot 行が MySQL の制約を通るかを返す。Postgres 版と同じく、
// DB が実際に拒否するものだけを見る。見るのは次の列の制約だけで、それ以外の列は NOT NULL か
// 型の範囲（INT・BIGINT・LONGTEXT）に収まるので、拒否されない。
//   - channel_type: VARCHAR(3) と CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY'))
//   - site: VARCHAR(255)。256 文字以上は strict モードで拒否される
//   - start_at / updated_at: DATETIME(6)。年が 1〜9999 の外は、接続の driver が書く前に拒否する（年 0 も
//     driver の範囲 [1, 9999] の外。年 10000 は MySQL が拒否する。検査: TestMySQLDatetimeRangeRejectsYearsAbove9999）
//
// **放送を同定できないことや空の名前では落とさない。** 行を落とすと FK 先を失う program_intents /
// program_overrides も連動して落ちる。それは「この番組を録れ / 録るな」というユーザーが明示した意図で、
// 導出できない。ここの責務は「1 行の INSERT が落ちて tx 全体が巻き戻るのを防ぐ」ことに限る。
// 検査: TestInsertableSnapshot（境界の行）と TestCatalogRestoreSkipsSnapshotsMySQLRejects（実 MySQL）。
func insertableSnapshot(s catalogdoc.ProgramSnapshot) bool {
	switch s.ChannelType {
	case "GR", "BS", "CS", "SKY":
	default:
		return false
	}
	return len([]rune(s.Site)) <= 255 && datetimeInRange(s.StartAt) && datetimeInRange(s.UpdatedAt)
}

// datetimeInRange は t が DATETIME(6) として書ける年（driver が受け入れる 1〜9999）にあるかを返す。
// 接続の時刻帯は UTC なので、判定も UTC で行う。
func datetimeInRange(t time.Time) bool {
	year := t.UTC().Year()
	return year >= 1 && year <= 9999
}

// applyDocument は 1 つの tx の中で catalogdoc.Document の各表を FK 順に復元する。
func applyDocument(ctx context.Context, tx *sql.Tx, doc *catalogdoc.Document, options store.RestoreOptions, siteNames []string) (*store.RestoreResult, error) {
	if err := validateCatalogMediaAssets(doc.MediaAssets); err != nil {
		return nil, err
	}
	res := &store.RestoreResult{}

	// 再評価の名前付きロックは最初の書き込みより前に取る（ロック順序は docs/data/store-contracts.md §直列化）。
	if err := lockLabelRuleReconcile(ctx, tx); err != nil {
		return nil, fmt.Errorf("locking label rule re-evaluation: %w", err)
	}
	if err := applyRules(ctx, tx, doc.Rules, res); err != nil {
		return nil, err
	}
	// 分類ルールは録画より前に書く（録画の INSERT のトリガーがその時点のルール集合で当たりを作る）。
	// 対象ルールのキーを先に外し、同じ文書の復元を冪等にする。
	for _, rule := range doc.Rules {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rule_import_keys WHERE rule_id = ?`, rule.ID); err != nil {
			return nil, fmt.Errorf("deleting rule import key: %w", err)
		}
	}
	for _, key := range doc.RuleImportKeys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rule_import_keys (rule_id, source, external_id) VALUES (?, ?, ?)`,
			key.RuleID, key.Source, key.ExternalID); err != nil {
			return nil, fmt.Errorf("restoring rule import key: %w", err)
		}
	}
	if err := applyLabelRules(ctx, tx, doc.LabelRules, res); err != nil {
		return nil, err
	}

	skipped, err := applyProgramSnapshots(ctx, tx, doc.ProgramSnapshots, res)
	if err != nil {
		return nil, err
	}
	if err := applyProgramIntents(ctx, tx, doc.ProgramIntents, skipped, res); err != nil {
		return nil, err
	}
	if err := applyProgramOverrides(ctx, tx, doc.ProgramOverrides, skipped, res); err != nil {
		return nil, err
	}
	if err := applyRecordings(ctx, tx, doc.Recordings, res); err != nil {
		return nil, err
	}
	if err := applyRecordingPurgeRequests(ctx, tx, doc.RecordingPurgeRequests, res); err != nil {
		return nil, err
	}
	if err := applyRecordingWatched(ctx, tx, doc.RecordingWatched, res); err != nil {
		return nil, err
	}
	if err := applyRecordingEncodePolicies(ctx, tx, doc.RecordingEncodePolicies, res); err != nil {
		return nil, err
	}
	if err := applyMediaAssets(ctx, tx, doc.MediaAssets, res, options.MissingAssetCopyKeys); err != nil {
		return nil, err
	}
	// チャプターの所有 2 表は media_assets の後（区間は所有の行を FK で指す）。
	if err := applyRecordingChapterOwnerships(ctx, tx, doc.RecordingChapterOwnerships, res); err != nil {
		return nil, err
	}
	if err := applyRecordingChapterSpans(ctx, tx, doc.RecordingChapterSpans, res); err != nil {
		return nil, err
	}
	if err := applyDropStats(ctx, tx, doc.DropStats, res); err != nil {
		return nil, err
	}
	if err := applyDropPositions(ctx, tx, doc.DropPositions, res); err != nil {
		return nil, err
	}
	if err := applyMediaAssetTSScans(ctx, tx, doc.MediaAssetTSScans, res); err != nil {
		return nil, err
	}

	// 当たりは導出値なので、復元したルール集合に対して全件を再評価して閉じる。
	if _, err := reevaluateLabelRules(ctx, tx); err != nil {
		return nil, fmt.Errorf("re-evaluating label rules after rescue: %w", err)
	}

	// 版の更新は最後に置く。ruler の書き込み tx の版照合は版の行ロックを取るので、
	// 先頭に置くと Restore の tx が終わるまで ruler のコミットが待たされる。
	// 版の行は site 順に 1 回ずつ進める（順序を固定して、並行する復元とのロック順を揃える）。
	for _, site := range sortedUniqueSites(siteNames) {
		if err := bumpRulerInputRevision(ctx, tx, site); err != nil {
			return nil, fmt.Errorf("bumping ruler input revisions after catalog restore: %w", err)
		}
	}
	return res, nil
}

// sortedUniqueSites は site を重複なく昇順に並べて返す（Postgres 版の bumpRulerInputRevisions と同じ）。
func sortedUniqueSites(siteNames []string) []string {
	sites := append([]string(nil), siteNames...)
	sort.Strings(sites)
	out := sites[:0]
	for i, site := range sites {
		if i > 0 && site == sites[i-1] {
			continue
		}
		out = append(out, site)
	}
	return out
}

// applyRules は rules とその従属表を復元し、件数を res.Rules に書く。
func applyRules(ctx context.Context, tx *sql.Tx, rules []catalogdoc.Rule, res *store.RestoreResult) error {
	for _, rule := range rules {
		if err := upsertRule(ctx, tx, rule); err != nil {
			return fmt.Errorf("upserting rule %d: %w", rule.ID, err)
		}
	}
	res.Rules = len(rules)
	return nil
}

// upsertRule は rule 1 件と子表を書く。子表は一度消してから入れ直し、削除された条件も反映する。
func upsertRule(ctx context.Context, tx *sql.Tx, rule catalogdoc.Rule) error {
	profiles, err := json.Marshal(nonNilStringSlice(rule.EncodeProfiles))
	if err != nil {
		return fmt.Errorf("encoding encode profiles: %w", err)
	}
	meta := rule.Metadata
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	if _, err := tx.ExecContext(ctx, `
 INSERT INTO rules (id, name, description, enabled, priority, is_free, duration_min_ms, duration_max_ms,
     period_start_at, period_end_at, dedupe_enabled, dedupe_threshold, dedupe_window,
     keep_original, encode_profiles, filename_template, metadata, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE
     name = new.name, description = new.description, enabled = new.enabled, priority = new.priority,
     is_free = new.is_free, duration_min_ms = new.duration_min_ms, duration_max_ms = new.duration_max_ms,
     period_start_at = new.period_start_at, period_end_at = new.period_end_at,
     dedupe_enabled = new.dedupe_enabled, dedupe_threshold = new.dedupe_threshold, dedupe_window = new.dedupe_window,
     keep_original = new.keep_original, encode_profiles = new.encode_profiles,
     filename_template = new.filename_template, metadata = new.metadata,
     created_at = new.created_at, updated_at = new.updated_at
 `, rule.ID, rule.Name, rule.Description, rule.Enabled, rule.Priority, optionalValue(rule.IsFree),
		optionalValue(rule.DurationMinMs), optionalValue(rule.DurationMaxMs),
		optionalValue(rule.PeriodStartAt), optionalValue(rule.PeriodEndAt),
		rule.DedupeEnabled, optionalValue(rule.DedupeThreshold), optionalValue(rule.DedupeWindowSeconds),
		rule.KeepOriginal, string(profiles), rule.FilenameTemplate, string(meta), rule.CreatedAt, rule.UpdatedAt,
	); err != nil {
		return err
	}

	for _, table := range []string{"rule_text_matches", "rule_services", "rule_channel_types", "rule_genres", "rule_times", "rule_sites"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rule_id = ?", rule.ID); err != nil {
			return fmt.Errorf("deleting %s: %w", table, err)
		}
	}
	// 子表の挿入は Postgres 版の ON CONFLICT に対応させる。子表の PK は（rule_id 以外に）一意キーを持たないので、
	// ON DUPLICATE KEY が狙わない行で発火することは無い。
	for _, m := range rule.TextMatches {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
 VALUES (?, ?, ?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE target = new.target, mode = new.mode, value = new.value,
     case_sensitive = new.case_sensitive, negate = new.negate`,
			rule.ID, m.Seq, m.Target, m.Mode, m.Value, m.CaseSensitive, m.Negate); err != nil {
			return fmt.Errorf("inserting text match seq=%d: %w", m.Seq, err)
		}
	}
	for _, s := range rule.Services {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_services (rule_id, network_id, service_id) VALUES (?, ?, ?)
 ON DUPLICATE KEY UPDATE rule_id = rule_id`, rule.ID, s.NetworkID, s.ServiceID); err != nil {
			return fmt.Errorf("inserting service %d/%d: %w", s.NetworkID, s.ServiceID, err)
		}
	}
	for _, ct := range rule.ChannelTypes {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_channel_types (rule_id, channel_type) VALUES (?, ?)
 ON DUPLICATE KEY UPDATE rule_id = rule_id`, rule.ID, ct); err != nil {
			return fmt.Errorf("inserting channel type %s: %w", ct, err)
		}
	}
	for _, g := range rule.Genres {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_genres (rule_id, genre_lv1) VALUES (?, ?)
 ON DUPLICATE KEY UPDATE rule_id = rule_id`, rule.ID, g); err != nil {
			return fmt.Errorf("inserting genre %d: %w", g, err)
		}
	}
	for _, t := range rule.Times {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_times (rule_id, seq, weekdays, start_sec, end_sec) VALUES (?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE weekdays = new.weekdays, start_sec = new.start_sec, end_sec = new.end_sec`,
			rule.ID, t.Seq, t.Weekdays, t.StartSec, t.EndSec); err != nil {
			return fmt.Errorf("inserting time seq=%d: %w", t.Seq, err)
		}
	}
	for _, site := range rule.Sites {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO rule_sites (rule_id, site) VALUES (?, ?)
 ON DUPLICATE KEY UPDATE rule_id = rule_id`, rule.ID, site); err != nil {
			return fmt.Errorf("inserting site %s: %w", site, err)
		}
	}
	return nil
}

// applyLabelRules は label_rules を id 保持で復元し、件数を res.LabelRules に書く。
// value_key は MySQL ではアプリが書く（生成列ではない）。value が series_key を持たなければ拒む。
func applyLabelRules(ctx context.Context, tx *sql.Tx, rules []catalogdoc.LabelRule, res *store.RestoreResult) error {
	for _, r := range rules {
		valueKey, err := labelRuleValueKey(r.Value)
		if err != nil {
			return fmt.Errorf("label_rule %d: %w", r.ID, err)
		}
		// label_rules の一意キーは PK だけなので、ON DUPLICATE KEY UPDATE が狙う行以外で発火しない。
		if _, err := tx.ExecContext(ctx, "INSERT INTO label_rules (id, `key`, value, keyword, priority, value_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS new ON DUPLICATE KEY UPDATE `key` = new.`key`, value = new.value, keyword = new.keyword, priority = new.priority, value_key = new.value_key, created_at = new.created_at, updated_at = new.updated_at",
			r.ID, r.Key, r.Value, r.Keyword, r.Priority, valueKey, r.CreatedAt, r.UpdatedAt); err != nil {
			return fmt.Errorf("upserting label_rule %d: %w", r.ID, err)
		}
	}
	res.LabelRules = len(rules)
	return nil
}

// applyProgramSnapshots は program_snapshots を復元し、件数を res.ProgramSnapshots /
// res.SkippedProgramSnapshots に書いて、復元できなかったキーを返す。
func applyProgramSnapshots(ctx context.Context, tx *sql.Tx, snapshots []catalogdoc.ProgramSnapshot, res *store.RestoreResult) (map[snapshotKey]struct{}, error) {
	skipped := make(map[snapshotKey]struct{})
	count, skippedCount := 0, 0
	for _, s := range snapshots {
		// 壊れた 1 行で災害復旧そのものを止めない。拒否される行をそのまま入れると tx 全体が巻き戻るので、
		// 放送と retention_grace で GC される導出データの行だけを落として、永続資産の復元を続ける。
		if !insertableSnapshot(s) {
			slog.Warn("rescue: skipping program_snapshot the database would reject",
				"site", s.Site, "program_id", s.ProgramID, "channel_type", s.ChannelType)
			skipped[snapshotKey{site: s.Site, programID: s.ProgramID}] = struct{}{}
			skippedCount++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO program_snapshots (site, program_id, title, start_at, duration_ms, network_id, service_id,
     channel_type, channel, event_id, service_name, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE title = new.title, start_at = new.start_at, duration_ms = new.duration_ms,
     network_id = new.network_id, service_id = new.service_id, channel_type = new.channel_type,
     channel = new.channel, event_id = new.event_id, service_name = new.service_name, updated_at = new.updated_at`,
			s.Site, s.ProgramID, s.Title, s.StartAt, s.DurationMs, s.NetworkID, s.ServiceID,
			s.ChannelType, s.Channel, s.EventID, s.ServiceName, s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("upserting program_snapshot %s/%d: %w", s.Site, s.ProgramID, err)
		}
		count++
	}
	res.ProgramSnapshots = count
	res.SkippedProgramSnapshots = skippedCount
	return skipped, nil
}

// applyProgramIntents は program_intents を復元し、件数を res.ProgramIntents / res.SkippedProgramIntents に書く。
func applyProgramIntents(ctx context.Context, tx *sql.Tx, intents []catalogdoc.ProgramIntent, skippedSnapshots map[snapshotKey]struct{}, res *store.RestoreResult) error {
	count, skipped := 0, 0
	for _, i := range intents {
		if _, missing := skippedSnapshots[snapshotKey{site: i.Site, programID: i.ProgramID}]; missing {
			skipped++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO program_intents (site, program_id, action, created_at, updated_at) VALUES (?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE action = new.action, created_at = new.created_at, updated_at = new.updated_at`,
			i.Site, i.ProgramID, i.Action, i.CreatedAt, i.UpdatedAt); err != nil {
			return fmt.Errorf("upserting program_intent %s/%d: %w", i.Site, i.ProgramID, err)
		}
		count++
	}
	res.ProgramIntents = count
	res.SkippedProgramIntents = skipped
	return nil
}

// applyProgramOverrides は program_overrides を復元し、件数を res.ProgramOverrides / res.SkippedProgramOverrides に書く。
func applyProgramOverrides(ctx context.Context, tx *sql.Tx, overrides []catalogdoc.ProgramOverride, skippedSnapshots map[snapshotKey]struct{}, res *store.RestoreResult) error {
	count, skipped := 0, 0
	for _, o := range overrides {
		if _, missing := skippedSnapshots[snapshotKey{site: o.Site, programID: o.ProgramID}]; missing {
			skipped++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO program_overrides (site, program_id, overrides, created_at, updated_at) VALUES (?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE overrides = new.overrides, created_at = new.created_at, updated_at = new.updated_at`,
			o.Site, o.ProgramID, string(o.Overrides), o.CreatedAt, o.UpdatedAt); err != nil {
			return fmt.Errorf("upserting program_override %s/%d: %w", o.Site, o.ProgramID, err)
		}
		count++
	}
	res.ProgramOverrides = count
	res.SkippedProgramOverrides = skipped
	return nil
}

// applyRecordings は recordings を復元し、件数を res.Recordings に書く。
func applyRecordings(ctx context.Context, tx *sql.Tx, recordings []catalogdoc.Recording, res *store.RestoreResult) error {
	for _, r := range recordings {
		if err := upsertRecording(ctx, tx, r); err != nil {
			return fmt.Errorf("upserting recording %d: %w", r.ID, err)
		}
	}
	res.Recordings = len(recordings)
	return nil
}

// upsertRecording は録画 1 件を id で書く。
// recordings は id 以外の一意キー（アクティブな同一番組の一意性）を持つので、ON DUPLICATE KEY UPDATE は使わない。
// 狙わない一意キーの衝突で別の行を黙って書き換えないよう、先に行の有無を読んでから UPDATE か INSERT を選ぶ。
// 衝突は INSERT / UPDATE のエラーとして返り、ErrConflict になる。
// 題名から series_key を、genres から genre_lv1 を、アプリが書く（MySQL では生成列でない）。
func upsertRecording(ctx context.Context, tx *sql.Tx, r catalogdoc.Recording) error {
	qe := r.QualityEvents
	if len(qe) == 0 || bytes.Equal(bytes.TrimSpace(qe), []byte("null")) {
		qe = []byte("[]")
	}
	// failureReported のない旧形式の catalog だけ、品質イベントの配列から列を埋める。
	reported := r.FailureReported
	if reported == nil {
		var events []struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(qe, &events); err != nil {
			return fmt.Errorf("migrating recording %d failure report: %w", r.ID, err)
		}
		value := false
		for _, event := range events {
			if event.Event == "recording.failed" {
				value = true
			}
		}
		reported = &value
	}
	var seriesKey any
	if key, ok := serieskey.SeriesKey(r.Title); ok {
		seriesKey = key
	}
	// genre_lv1 は genres から導く。規則は EPG の射影と共通の epgGenreLevels に 1 つだけ置く。
	genreLv1 := epgGenreLevels(r.Genres)

	var locked int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM recordings WHERE id = ? FOR UPDATE`, r.ID).Scan(&locked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
 INSERT INTO recordings (id, rule_id, source, site, network_id, service_id, event_id, service_name,
     channel_type, channel, title, description, extended, genres, is_free, program_start_at, program_duration_ms,
     status, started_at, ended_at, quality_events, failure_reported, deleted_at, superseded_at, purged_at,
     created_at, updated_at, genre_lv1, series_key)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, optionalValue(r.RuleID), r.Source, r.Site, r.NetworkID, r.ServiceID, r.EventID, r.ServiceName,
			r.ChannelType, r.Channel, r.Title, optionalValue(r.Description), optionalBytes(r.Extended), optionalBytes(r.Genres),
			r.IsFree, r.ProgramStartAt, r.ProgramDurationMs, r.Status, optionalValue(r.StartedAt), optionalValue(r.EndedAt),
			string(qe), *reported, optionalValue(r.DeletedAt), optionalValue(r.SupersededAt), optionalValue(r.PurgedAt),
			r.CreatedAt, r.UpdatedAt, genreLv1, seriesKey)
	case err == nil:
		_, err = tx.ExecContext(ctx, `
 UPDATE recordings SET rule_id = ?, source = ?, site = ?, network_id = ?, service_id = ?, event_id = ?,
     service_name = ?, channel_type = ?, channel = ?, title = ?, description = ?, extended = ?, genres = ?,
     is_free = ?, program_start_at = ?, program_duration_ms = ?, status = ?, started_at = ?, ended_at = ?,
     quality_events = ?, failure_reported = ?, deleted_at = ?, superseded_at = ?, purged_at = ?,
     created_at = ?, updated_at = ?, genre_lv1 = ?, series_key = ?
 WHERE id = ?`,
			optionalValue(r.RuleID), r.Source, r.Site, r.NetworkID, r.ServiceID, r.EventID,
			r.ServiceName, r.ChannelType, r.Channel, r.Title, optionalValue(r.Description), optionalBytes(r.Extended), optionalBytes(r.Genres),
			r.IsFree, r.ProgramStartAt, r.ProgramDurationMs, r.Status, optionalValue(r.StartedAt), optionalValue(r.EndedAt),
			string(qe), *reported, optionalValue(r.DeletedAt), optionalValue(r.SupersededAt), optionalValue(r.PurgedAt),
			r.CreatedAt, r.UpdatedAt, genreLv1, seriesKey, r.ID)
	}
	if err != nil {
		return err
	}
	return nil
}

// applyRecordingPurgeRequests は recording_purge_requests を復元し、件数を res.RecordingPurgeRequests に書く。
func applyRecordingPurgeRequests(ctx context.Context, tx *sql.Tx, requests []catalogdoc.RecordingPurgeRequest, res *store.RestoreResult) error {
	for _, p := range requests {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO recording_purge_requests (recording_id, requested_at) VALUES (?, ?) AS new
 ON DUPLICATE KEY UPDATE requested_at = new.requested_at`, p.RecordingID, p.RequestedAt); err != nil {
			return fmt.Errorf("upserting recording_purge_request %d: %w", p.RecordingID, err)
		}
	}
	res.RecordingPurgeRequests = len(requests)
	return nil
}

func applyRecordingWatched(ctx context.Context, tx *sql.Tx, rows []catalogdoc.RecordingWatched, res *store.RestoreResult) error {
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO recording_watched (recording_id, watched_at) VALUES (?, ?) AS new
 ON DUPLICATE KEY UPDATE watched_at = new.watched_at`, row.RecordingID, row.WatchedAt); err != nil {
			return fmt.Errorf("upserting recording_watched %d: %w", row.RecordingID, err)
		}
	}
	res.RecordingWatched = len(rows)
	return nil
}

// applyRecordingEncodePolicies は recording_processing_policy を復元し、件数を res.RecordingEncodePolicies に書く。
func applyRecordingEncodePolicies(ctx context.Context, tx *sql.Tx, policies []catalogdoc.RecordingEncodePolicy, res *store.RestoreResult) error {
	for _, p := range policies {
		profiles, err := json.Marshal(nonNilStringSlice(p.EncodeProfiles))
		if err != nil {
			return fmt.Errorf("encoding encode profiles of %d: %w", p.RecordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, created_at, updated_at, cm_detect)
 VALUES (?, ?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE keep_original = new.keep_original, encode_profiles = new.encode_profiles,
     created_at = new.created_at, updated_at = new.updated_at, cm_detect = new.cm_detect`,
			p.RecordingID, p.KeepOriginal, string(profiles), p.CreatedAt, p.UpdatedAt, p.CMDetect); err != nil {
			return fmt.Errorf("upserting recording_processing_policy %d: %w", p.RecordingID, err)
		}
	}
	res.RecordingEncodePolicies = len(policies)
	return nil
}

// validateCatalogMediaAssets は現行 export では必須の物理コピー情報が欠けた文書を拒否する。
func validateCatalogMediaAssets(assets []catalogdoc.MediaAsset) error {
	for _, asset := range assets {
		if asset.Store == "" {
			return fmt.Errorf("media_asset %d is missing store", asset.ID)
		}
		if asset.Kind == "original" {
			continue
		}
		if asset.DerivedGeneration == nil {
			return fmt.Errorf("derived media_asset %d is missing derivedGeneration", asset.ID)
		}
		if asset.Generation == nil {
			return fmt.Errorf("derived media_asset %d is missing generation", asset.ID)
		}
	}
	return nil
}

// applyRecordingChapterOwnerships は recording_chapter_ownership を復元し、件数を res.RecordingChapterOwnerships に書く。
func applyRecordingChapterOwnerships(ctx context.Context, tx *sql.Tx, rows []catalogdoc.RecordingChapterOwnership, res *store.RestoreResult) error {
	for _, o := range rows {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO recording_chapter_ownership (recording_id, adopted_at) VALUES (?, ?) AS new
 ON DUPLICATE KEY UPDATE adopted_at = new.adopted_at`, o.RecordingID, o.AdoptedAt); err != nil {
			return fmt.Errorf("upserting recording_chapter_ownership %d: %w", o.RecordingID, err)
		}
	}
	res.RecordingChapterOwnerships = len(rows)
	return nil
}

// applyRecordingChapterSpans は recording_chapter_spans を復元し、件数を res.RecordingChapterSpans に書く。
// 足すだけで消さない。Postgres 版の排他制約（同じ録画の区間は重ならない）に当たる区間は、
// ON CONFLICT DO NOTHING と同じく黙って飛ばす。先に入った区間が残る。同一区間も重なりに含むので、再実行で二重にならない。
// MySQL の表は部分的な重なりを拒まないので、この経路はここで重なりを確かめる。
// Replace は checkSpansDisjoint で確かめる。
func applyRecordingChapterSpans(ctx context.Context, tx *sql.Tx, rows []catalogdoc.RecordingChapterSpan, res *store.RestoreResult) error {
	for _, s := range rows {
		var overlapping int
		// 区間は [start_ms, end_ms) なので、重なりは start_ms < 既存の end_ms かつ 既存の start_ms < end_ms。
		if err := tx.QueryRowContext(ctx, `
 SELECT COUNT(*) FROM recording_chapter_spans
 WHERE recording_id = ? AND start_ms < ? AND end_ms > ?`,
			s.RecordingID, s.EndMs, s.StartMs).Scan(&overlapping); err != nil {
			return fmt.Errorf("checking recording_chapter_span [%d,%d) of %d: %w", s.StartMs, s.EndMs, s.RecordingID, err)
		}
		if overlapping > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO recording_chapter_spans (recording_id, start_ms, end_ms, label, cut) VALUES (?, ?, ?, ?, ?)`,
			s.RecordingID, s.StartMs, s.EndMs, optionalValue(s.Label), s.Cut); err != nil {
			return fmt.Errorf("inserting recording_chapter_span [%d,%d) of %d: %w", s.StartMs, s.EndMs, s.RecordingID, err)
		}
	}
	res.RecordingChapterSpans = len(rows)
	return nil
}

// applyDropStats は drop_stats を復元し、件数を res.DropStats に書く。
func applyDropStats(ctx context.Context, tx *sql.Tx, stats []catalogdoc.DropStat, res *store.RestoreResult) error {
	for _, d := range stats {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
 VALUES (?, ?, ?, ?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE packets = new.packets, drops = new.drops, errors = new.errors,
     scrambled = new.scrambled, pid_type = new.pid_type`,
			d.MediaAssetID, d.Pid, d.Packets, d.Drops, d.Errors, d.Scrambled, optionalValue(d.PidType)); err != nil {
			return fmt.Errorf("upserting drop_stat asset=%d pid=%d: %w", d.MediaAssetID, d.Pid, err)
		}
	}
	res.DropStats = len(stats)
	return nil
}

// applyDropPositions は drop_positions を復元し、件数を res.DropPositions に書く。
func applyDropPositions(ctx context.Context, tx *sql.Tx, positions []catalogdoc.DropPosition, res *store.RestoreResult) error {
	for _, p := range positions {
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms) VALUES (?, ?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE pid = new.pid, elapsed_ms = new.elapsed_ms`,
			p.MediaAssetID, p.ByteOffset, p.Pid, optionalValue(p.ElapsedMs)); err != nil {
			return fmt.Errorf("upserting drop_position asset=%d offset=%d: %w", p.MediaAssetID, p.ByteOffset, err)
		}
	}
	res.DropPositions = len(positions)
	return nil
}

// applyMediaAssetTSScans は media_asset_ts_scans を復元し、件数を res.MediaAssetTSScans に書く。
// 計測の内容のリビジョンは、復元後の media_assets の値を写す（Postgres 版の INSERT ... SELECT と同じ）。
// media_assets の行が無い計測は書かない。
func applyMediaAssetTSScans(ctx context.Context, tx *sql.Tx, scans []catalogdoc.MediaAssetTSScan, res *store.RestoreResult) error {
	for _, s := range scans {
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT content_revision FROM media_assets WHERE id = ?`, s.MediaAssetID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading content revision of asset %d: %w", s.MediaAssetID, err)
		}
		if _, err := tx.ExecContext(ctx, `
 INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes, content_revision) VALUES (?, ?, ?) AS new
 ON DUPLICATE KEY UPDATE scanned_size_bytes = new.scanned_size_bytes, content_revision = new.content_revision`,
			s.MediaAssetID, s.ScannedSizeBytes, revision); err != nil {
			return fmt.Errorf("upserting media_asset_ts_scan asset=%d: %w", s.MediaAssetID, err)
		}
	}
	res.MediaAssetTSScans = len(scans)
	return nil
}

// optionalValue は nil を SQL の NULL に、非 nil のポインタを指す値にする。
func optionalValue[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// optionalBytes は空の JSON を SQL の NULL にする。
func optionalBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nonNilStringSlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
