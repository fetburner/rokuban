package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// Tuners は MySQL でチューナー同期・一覧・容量読み取りを実装する。
type Tuners struct {
	db *sql.DB
}

// NewTuners は MySQL を使う Tuners を作る。
func NewTuners(db *sql.DB) *Tuners {
	return &Tuners{db: db}
}

var _ store.TunerSync = (*Tuners)(nil)
var _ store.TunerReader = (*Tuners)(nil)
var _ store.Capacity = (*Tuners)(nil)

// SweepMark は stale 判定の基準となる DB 時刻を返す。
// observed_at と同じ精度（マイクロ秒）で返すので、境界の行を正しく残せる。
func (s *Tuners) SweepMark(ctx context.Context) (time.Time, error) {
	var mark time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(6)").Scan(&mark); err != nil {
		return time.Time{}, fmt.Errorf("getting tuner sweep mark: %w", err)
	}
	return mark, nil
}

const upsertTunerSync = `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault, observed_at)
VALUES `

const updateTunerSync = `
AS incoming ON DUPLICATE KEY UPDATE
    name = incoming.name, types = incoming.types,
    is_available = incoming.is_available, is_fault = incoming.is_fault,
    observed_at = incoming.observed_at`

// Upsert は site のチューナー一覧を 1 tx の 1 文で反映する。
// 1 件でも CHECK に違反したら文ごと失敗し、全件を反映しない。
func (s *Tuners) Upsert(ctx context.Context, site string, tuners []store.TunerInput) error {
	if len(tuners) == 0 {
		return nil
	}
	args := make([]any, 0, len(tuners)*6)
	for _, tuner := range tuners {
		types, err := json.Marshal(tuner.Types)
		if err != nil {
			return fmt.Errorf("encoding tuner types for site %s: %w", site, err)
		}
		args = append(args, site, tuner.Index, tuner.Name, string(types), tuner.IsAvailable, tuner.IsFault)
	}
	const tuple = "(?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6))"
	query := upsertTunerSync + strings.Repeat(tuple+", ", len(tuners)-1) + tuple + updateTunerSync
	if err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	}); err != nil {
		return fmt.Errorf("upserting tuners for site %s: %w", site, err)
	}
	return nil
}

// DeleteStale は observedBefore より古い site の行を削除し、削除件数を返す。
func (s *Tuners) DeleteStale(ctx context.Context, site string, observedBefore time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM tuner_sync WHERE site = ? AND observed_at < ?`, site, observedBefore)
	if err != nil {
		return 0, fmt.Errorf("deleting stale tuners for site %s: %w", site, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting stale tuners for site %s: %w", site, err)
	}
	return deleted, nil
}

// List は site の tuner_sync 行を index 順で返す。
func (s *Tuners) List(ctx context.Context, site string) ([]store.Tuner, error) {
	return queryRows(ctx, s.db, "tuner projection", tunerSelect+` WHERE site = ? ORDER BY tuner_index`,
		scanTuner, site)
}

const tunerSelect = `SELECT site, tuner_index, name, types, is_available, is_fault, observed_at FROM tuner_sync`

func scanTuner(rows *sql.Rows) (store.Tuner, error) {
	var t store.Tuner
	var types []byte
	if err := rows.Scan(&t.Site, &t.Index, &t.Name, &types, &t.IsAvailable, &t.IsFault, &t.ObservedAt); err != nil {
		return store.Tuner{}, err
	}
	if err := json.Unmarshal(types, &t.Types); err != nil {
		return store.Tuner{}, fmt.Errorf("decoding tuner types: %w", err)
	}
	return t, nil
}

// capacityDemandSelect は予約の需要を 1 行ずつ返す。ReadSite と ReadAllSites が共有する。
// 終了端は開始時刻からミリ秒単位の尺を足した値で、秒未満を落とさない。
const capacityDemandSelect = `
SELECT r.site, r.rule_id, s.channel_type, s.channel, s.start_at,
       TIMESTAMPADD(MICROSECOND, s.duration_ms * 1000, s.start_at),
       r.base, i.action, o.overrides
` + reservationJoins + `
WHERE NOT ` + reservationMissingEvent

// ReadSite は site の需要とチューナーを返す。
func (s *Tuners) ReadSite(ctx context.Context, site string) (store.CapacityInput, error) {
	demands, err := queryRows(ctx, s.db, "capacity demand", capacityDemandSelect+` AND r.site = ? ORDER BY s.start_at`,
		scanCapacityDemand, site)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing capacity demand for site %s: %w", site, err)
	}
	tuners, err := s.List(ctx, site)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing tuner projection for site %s: %w", site, err)
	}
	return store.CapacityInput{Demands: demands, Tuners: tuners}, nil
}

// ReadAllSites は全 site の需要とチューナーを返す。
func (s *Tuners) ReadAllSites(ctx context.Context) (store.CapacityInput, error) {
	demands, err := queryRows(ctx, s.db, "capacity demand", capacityDemandSelect+` ORDER BY r.site, s.start_at`,
		scanCapacityDemand)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing capacity demand for all sites: %w", err)
	}
	tuners, err := queryRows(ctx, s.db, "tuner projection", tunerSelect+` ORDER BY site, tuner_index`, scanTuner)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing tuner projection for all sites: %w", err)
	}
	return store.CapacityInput{Demands: demands, Tuners: tuners}, nil
}

func scanCapacityDemand(rows *sql.Rows) (store.CapacityDemand, error) {
	var d store.CapacityDemand
	var base, overrides []byte
	if err := rows.Scan(&d.Site, &d.RuleID, &d.ChannelType, &d.Channel, &d.StartAt, &d.EndAt,
		&base, &d.IntentAction, &overrides); err != nil {
		return store.CapacityDemand{}, err
	}
	d.Base, d.Overrides = base, overrides
	return d, nil
}

// PreviewRule は保存前プレビューの対象ルールの状態を返す。
func (s *Tuners) PreviewRule(ctx context.Context, ruleID int64) (store.CapacityPreviewRule, error) {
	var rule store.CapacityPreviewRule
	err := s.db.QueryRowContext(ctx, `SELECT enabled, dedupe_enabled FROM rules WHERE id = ?`, ruleID).
		Scan(&rule.Enabled, &rule.DedupeEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CapacityPreviewRule{}, fmt.Errorf("rule %d: %w", ruleID, store.ErrNotFound)
	}
	if err != nil {
		return store.CapacityPreviewRule{}, fmt.Errorf("loading rule %d for capacity preview: %w", ruleID, err)
	}
	return rule, nil
}

// PreviewCandidates は refs のうち、容量需要になりうる候補を返す。
// 編集中のルール（ruleID）が既に持つ予約は除外の対象に数えない。
func (s *Tuners) PreviewCandidates(ctx context.Context, refs []store.CapacityProgramRef, ruleID *int64) ([]store.CapacityPreviewCandidate, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	// refs は 1 つの JSON 引数で渡す。プレースホルダを refs の件数だけ作ると MySQL の上限（65535）に掛かる。
	type ref struct {
		Site      string `json:"site"`
		ProgramID int64  `json:"program_id"`
	}
	wire := make([]ref, len(refs))
	for i, r := range refs {
		wire[i] = ref{Site: r.Site, ProgramID: r.ProgramID}
	}
	refsJSON, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encoding capacity preview refs: %w", err)
	}
	args := []any{string(refsJSON), ruleID, ruleID}
	// site の照合順序は table 列と同じ utf8mb4_0900_as_cs にする。既定の照合順序だと大文字小文字を区別しない比較になりうる。
	query := `
SELECT p.site, p.program_id, i.action, s.channel_type, s.channel, p.start_at,
       TIMESTAMPADD(MICROSECOND, p.duration_ms * 1000, p.start_at)
FROM JSON_TABLE(?, '$[*]' COLUMNS (
         site VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs PATH '$.site',
         program_id BIGINT PATH '$.program_id'
     )) AS w
JOIN epg_programs p ON p.site = w.site AND p.program_id = w.program_id
JOIN epg_services s ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
LEFT JOIN program_intents i ON i.site = p.site AND i.program_id = p.program_id
WHERE (i.action IS NULL OR i.action <> 'skip')
  AND TIMESTAMPADD(MICROSECOND, p.duration_ms * 1000, p.start_at) > NOW(6)
  AND NOT EXISTS (
      SELECT 1 FROM reservations r
      WHERE r.site = p.site AND r.program_id = p.program_id
        AND (? IS NULL OR NOT (r.rule_id <=> ?))
  )
  AND NOT EXISTS (
      SELECT 1 FROM recordings rec
      JOIN media_assets a ON a.recording_id = rec.id AND a.kind = 'original'
      WHERE rec.site = p.site AND rec.network_id = p.network_id
        AND rec.service_id = p.service_id AND rec.event_id = p.event_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM never_scheduled_events nse
      WHERE nse.site = p.site AND nse.network_id = p.network_id
        AND nse.service_id = p.service_id AND nse.event_id = p.event_id
        AND nse.program_start_at = p.start_at
  )
ORDER BY p.site, p.start_at, p.program_id`
	return queryRows(ctx, s.db, "capacity preview candidates", query, func(rows *sql.Rows) (store.CapacityPreviewCandidate, error) {
		var c store.CapacityPreviewCandidate
		err := rows.Scan(&c.Site, &c.ProgramID, &c.IntentAction, &c.ChannelType, &c.Channel, &c.StartAt, &c.EndAt)
		return c, err
	}, args...)
}
