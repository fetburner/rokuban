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
	"github.com/fetburner/rokuban/internal/trgm"
)

// ListEnabled は有効なルール本体を優先度降順・ID 昇順で返す。
func (s *Reservations) ListEnabled(ctx context.Context) ([]store.Rule, error) {
	return queryRows(ctx, s.db, "enabled rules", `
 SELECT id, name, description, enabled, priority, is_free, duration_min_ms, duration_max_ms,
        period_start_at, period_end_at, dedupe_enabled, dedupe_threshold, dedupe_window,
        keep_original, encode_profiles, filename_template, metadata, created_at, updated_at
 FROM rules WHERE enabled = TRUE ORDER BY priority DESC, id
 `, func(rows *sql.Rows) (store.Rule, error) {
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
	})
}

// ListProgramIDReuses は終了スナップショットと射影の開始時刻から番組 ID の再利用候補を返す。
func (s *Reservations) ListProgramIDReuses(ctx context.Context, site string, now time.Time) ([]store.RulerProgramIDReuse, error) {
	return queryRows(ctx, s.db, "program ID reuses", `
 SELECT s.program_id, s.start_at, p.start_at
 FROM program_snapshots s JOIN epg_programs p USING (site, program_id)
 WHERE s.site = ?
   AND TIMESTAMPADD(MICROSECOND, s.duration_ms * 1000, s.start_at) < ?
   AND p.start_at > TIMESTAMPADD(HOUR, 24, s.start_at)
 `, func(rows *sql.Rows) (store.RulerProgramIDReuse, error) {
		var r store.RulerProgramIDReuse
		err := rows.Scan(&r.ProgramID, &r.SnapshotStartAt, &r.ProjectionStartAt)
		return r, err
	}, site, now)
}

// ListProgramIntents は指定サイトの利用者意図を返す。
func (s *Reservations) ListProgramIntents(ctx context.Context, site string) ([]store.RulerProgramIntent, error) {
	return queryRows(ctx, s.db, "program intents", `SELECT program_id, action FROM program_intents WHERE site = ?`,
		func(rows *sql.Rows) (store.RulerProgramIntent, error) {
			var r store.RulerProgramIntent
			err := rows.Scan(&r.ProgramID, &r.Action)
			return r, err
		}, site)
}

// ListInvestmentProgramIDs は指定サイトで record 意図または上書きのある番組 ID を返す。
func (s *Reservations) ListInvestmentProgramIDs(ctx context.Context, site string) ([]int64, error) {
	return queryRows(ctx, s.db, "program investments", `SELECT program_id FROM program_investments WHERE site = ?`, scanProgramID, site)
}

const programFulfilled = `EXISTS (
 SELECT 1 FROM recordings rec
 JOIN media_assets a ON a.recording_id = rec.id AND a.kind = 'original'
 WHERE rec.site = s.site AND rec.network_id = s.network_id
   AND rec.service_id = s.service_id AND rec.event_id = s.event_id
)`

// ListFulfilledProgramIDs は原本がコミットされた放送イベントの番組 ID を返す。
func (s *Reservations) ListFulfilledProgramIDs(ctx context.Context, site string) ([]int64, error) {
	return queryRows(ctx, s.db, "fulfilled programs", `
 SELECT s.program_id FROM program_snapshots s WHERE s.site = ? AND `+programFulfilled, scanProgramID, site)
}

// ListReservationProgramIDs は指定サイトの予約の番組 ID を返す。
func (s *Reservations) ListReservationProgramIDs(ctx context.Context, site string) ([]int64, error) {
	return queryRows(ctx, s.db, "reservation program IDs", `SELECT program_id FROM reservations WHERE site = ?`, scanProgramID, site)
}

// ListProjectedProgramIDs は指定した番組 ID のうち現時点で射影にあるものを返す。
func (s *Reservations) ListProjectedProgramIDs(ctx context.Context, site string, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return queryRows(ctx, s.db, "projected program IDs", `
 SELECT p.program_id FROM epg_programs p WHERE p.site = ?
   AND p.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
 `, scanProgramID, site, programIDsJSON(ids))
}

func scanProgramID(rows *sql.Rows) (int64, error) {
	var id int64
	err := rows.Scan(&id)
	return id, err
}

func programIDsJSON(ids []int64) string {
	payload := []byte{'['}
	for i, id := range ids {
		if i > 0 {
			payload = append(payload, ',')
		}
		payload = strconv.AppendInt(payload, id, 10)
	}
	return string(append(payload, ']'))
}

type rulerDedupeInput struct {
	ProgramID int64 `json:"program_id"`
	RuleID    int64 `json:"rule_id"`
}

// EvaluateDedupe は候補の最良一致を Go で選ぶ。同率は録画 ID の昇順で比較する。
// ごみ箱に入った完了済み録画も重複排除の根拠に含める。
func (s *Reservations) EvaluateDedupe(ctx context.Context, site string, candidates []store.RulerDedupeCandidate, now time.Time) (map[int64]store.RulerDedupeMatch, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	inputs := make([]rulerDedupeInput, 0, len(candidates))
	for _, c := range candidates {
		inputs = append(inputs, rulerDedupeInput(c))
	}
	payload, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("encoding dedupe candidates: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
 SELECT c.program_id, rec.id, p.name, rec.title, ru.dedupe_threshold
 FROM JSON_TABLE(?, '$[*]' COLUMNS (
   program_id BIGINT PATH '$.program_id', rule_id BIGINT PATH '$.rule_id'
 )) AS c
 JOIN rules ru ON ru.id = c.rule_id AND ru.dedupe_enabled
 JOIN epg_programs p ON p.site = ? AND p.program_id = c.program_id
 JOIN recordings rec ON rec.rule_id = c.rule_id AND rec.status = 'finished'
   AND NOT ((rec.network_id, rec.service_id, rec.event_id) <=> (p.network_id, p.service_id, p.event_id))
   AND (ru.dedupe_window IS NULL OR rec.program_start_at >= TIMESTAMPADD(SECOND, -ru.dedupe_window, CAST(? AS DATETIME(6))))
 `, string(payload), site, now)
	if err != nil {
		return nil, fmt.Errorf("querying dedupe history at %q: %w", site, err)
	}
	defer func() { _ = rows.Close() }()
	matches := make(map[int64]store.RulerDedupeMatch)
	for rows.Next() {
		var programID, recordingID int64
		var title, recordedTitle string
		var threshold float32
		if err := rows.Scan(&programID, &recordingID, &title, &recordedTitle, &threshold); err != nil {
			return nil, fmt.Errorf("scanning dedupe history: %w", err)
		}
		similarity := trgm.Similarity(title, recordedTitle)
		if similarity < threshold {
			continue
		}
		best, found := matches[programID]
		if !found || similarity > best.Similarity || (similarity == best.Similarity && recordingID < best.RecordingID) {
			matches[programID] = store.RulerDedupeMatch{RecordingID: recordingID, Similarity: similarity}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading dedupe history: %w", err)
	}
	return matches, nil
}

var errRulerInputChanged = errors.New("ruler input revision changed")

// ApplyPass は予約・スナップショット・ブレーカーを 1 tx で反映し、確定前に入力版を照合する。
func (s *Reservations) ApplyPass(ctx context.Context, site string, plan store.RulerPassPlan) (store.RulerPassResult, error) {
	var result store.RulerPassResult
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := upsertProgramSnapshots(ctx, tx, site, plan.SnapshotProgramIDs, now); err != nil {
			return err
		}
		locked, err := lockRulerPrograms(ctx, tx, site, plan)
		if err != nil {
			return err
		}
		if err := upsertRulerReservations(ctx, tx, site, plan, locked, now, &result); err != nil {
			return err
		}
		if len(plan.FulfilledProgramIDs) > 0 {
			result.FulfilledDeleted, err = deleteFulfilledReservations(ctx, tx, site, plan.FulfilledProgramIDs)
			if err != nil {
				return err
			}
		}
		if len(plan.DeleteProgramIDs) > 0 {
			if err := applyRulerDeletes(ctx, tx, site, plan, now, &result); err != nil {
				return err
			}
		}
		// 意図・上書きの書き手と同じ順で、番組の書き込みを済ませてから版をロックする。
		revision, err := lockRulerInputRevision(ctx, tx, site)
		if err != nil {
			return err
		}
		if revision != plan.InputRevision {
			return errRulerInputChanged
		}
		return nil
	})
	// runTx が rollback の失敗を結合した場合は、入力の再読込で隠さずエラーを返す。
	_, hasJoinedErrors := errors.AsType[interface {
		error
		Unwrap() []error
	}](err)
	if errors.Is(err, errRulerInputChanged) && !hasJoinedErrors {
		return store.RulerPassResult{InputRevisionChanged: true}, nil
	}
	if err != nil {
		return store.RulerPassResult{}, fmt.Errorf("applying ruler pass for %q: %w", site, err)
	}
	return result, nil
}

type rulerReservationInput struct {
	ProgramID             int64           `json:"program_id"`
	RuleID                *int64          `json:"rule_id"`
	Base                  json.RawMessage `json:"base"`
	DedupMatchRecordingID *int64          `json:"dedup_match_recording_id"`
	DedupSimilarity       *float32        `json:"dedup_similarity"`
}

const rulerReservationTable = `JSON_TABLE(?, '$[*]' COLUMNS (
 program_id BIGINT PATH '$.program_id',
 rule_id BIGINT PATH '$.rule_id',
 base JSON PATH '$.base',
 dedup_match_recording_id BIGINT PATH '$.dedup_match_recording_id',
 dedup_similarity FLOAT PATH '$.dedup_similarity'
))`

const reservationChanged = `(
 NOT (r.rule_id <=> d.rule_id)
 OR (d.rule_id IS NOT NULL AND NOT (
   r.base <=> d.base
   AND r.dedup_match_recording_id <=> d.dedup_match_recording_id
   AND r.dedup_similarity <=> d.dedup_similarity
 ))
)`

// lockRulerPrograms は予約の差分と削除を確定するまで、対象の番組行を保持する。
func lockRulerPrograms(ctx context.Context, tx *sql.Tx, site string, plan store.RulerPassPlan) ([]int64, error) {
	ids := make([]int64, 0, len(plan.Reservations)+len(plan.FulfilledProgramIDs)+len(plan.DeleteProgramIDs))
	for _, r := range plan.Reservations {
		ids = append(ids, r.ProgramID)
	}
	ids = append(ids, plan.FulfilledProgramIDs...)
	ids = append(ids, plan.DeleteProgramIDs...)
	if len(ids) == 0 {
		return nil, nil
	}
	return queryRows(ctx, tx, "ruler program locks", `
 SELECT s.program_id FROM program_snapshots s WHERE s.site = ?
   AND s.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
 ORDER BY s.program_id FOR UPDATE
 `, scanProgramID, site, programIDsJSON(ids))
}

// upsertRulerReservations はロックした番組の予約を書き、変更した ID を数える。
func upsertRulerReservations(ctx context.Context, tx *sql.Tx, site string, plan store.RulerPassPlan, locked []int64, now time.Time, result *store.RulerPassResult) error {
	if len(plan.Reservations) == 0 {
		return nil
	}
	materializableSet := rulerProgramIDSet(locked)
	inputs := make([]rulerReservationInput, 0, len(plan.Reservations))
	for _, r := range plan.Reservations {
		if _, ok := materializableSet[r.ProgramID]; !ok {
			result.UnmaterializedProgramIDs = append(result.UnmaterializedProgramIDs, r.ProgramID)
			continue
		}
		inputs = append(inputs, rulerReservationInput(r))
	}
	if len(inputs) == 0 {
		return nil
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return fmt.Errorf("encoding reservation inputs: %w", err)
	}
	payload := string(encoded)
	changed, err := queryRows(ctx, tx, "changed reservations", `
 SELECT d.program_id FROM `+rulerReservationTable+` AS d
 LEFT JOIN reservations r ON r.site = ? AND r.program_id = d.program_id
 WHERE r.program_id IS NULL OR `+reservationChanged, scanProgramID, payload, site)
	if err != nil || len(changed) == 0 {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
 UPDATE reservations r JOIN `+rulerReservationTable+` AS d ON d.program_id = r.program_id
 SET r.rule_id = d.rule_id,
     r.base = IF(d.rule_id IS NOT NULL, d.base, r.base),
     r.dedup_match_recording_id = IF(d.rule_id IS NOT NULL, d.dedup_match_recording_id, r.dedup_match_recording_id),
     r.dedup_similarity = IF(d.rule_id IS NOT NULL, d.dedup_similarity, r.dedup_similarity),
     r.updated_at = ?
 WHERE r.site = ? AND `+reservationChanged, payload, now, site); err != nil {
		return fmt.Errorf("updating changed reservations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
 INSERT INTO reservations (site, program_id, rule_id, base, dedup_match_recording_id, dedup_similarity, created_at, updated_at)
 SELECT ?, d.program_id, d.rule_id,
        IF(d.rule_id IS NOT NULL, d.base, NULL),
        IF(d.rule_id IS NOT NULL, d.dedup_match_recording_id, NULL),
        IF(d.rule_id IS NOT NULL, d.dedup_similarity, NULL), ?, ?
 FROM `+rulerReservationTable+` AS d
 LEFT JOIN reservations r ON r.site = ? AND r.program_id = d.program_id
 WHERE r.program_id IS NULL
 `, site, now, now, payload, site); err != nil {
		return fmt.Errorf("inserting new reservations: %w", err)
	}

	existing := rulerProgramIDSet(plan.ExistingProgramIDs)
	for _, id := range changed {
		if _, ok := existing[id]; ok {
			result.Updated++
		} else {
			result.Created++
		}
	}
	return nil
}

func rulerProgramIDSet(ids []int64) map[int64]struct{} {
	set := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

func deleteFulfilledReservations(ctx context.Context, tx *sql.Tx, site string, ids []int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `
 DELETE r FROM reservations r JOIN program_snapshots s USING (site, program_id)
 WHERE r.site = ? AND r.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
   AND `+programFulfilled, site, programIDsJSON(ids))
	if err != nil {
		return 0, fmt.Errorf("deleting fulfilled reservations: %w", err)
	}
	return result.RowsAffected()
}

const noProgramInvestment = `NOT EXISTS (
 SELECT 1 FROM program_investments v WHERE v.site = r.site AND v.program_id = r.program_id
)`

const releasedReservation = noProgramInvestment + ` AND (
 r.rule_id IS NULL OR EXISTS (
   SELECT 1 FROM program_intents i WHERE i.site = r.site AND i.program_id = r.program_id AND i.action = 'skip'
 )
)`

func applyRulerDeletes(ctx context.Context, tx *sql.Tx, site string, plan store.RulerPassPlan, now time.Time, result *store.RulerPassResult) error {
	released, err := queryRows(ctx, tx, "released reservations", `
 SELECT r.program_id FROM reservations r
 WHERE r.site = ? AND r.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
   AND `+releasedReservation+`
 FOR UPDATE
 `, scanProgramID, site, programIDsJSON(plan.DeleteProgramIDs))
	if err != nil {
		return err
	}
	if len(released) > 0 {
		if _, err := tx.ExecContext(ctx, `
  DELETE FROM reservations AS r WHERE r.site = ?
    AND r.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
    AND `+releasedReservation, site, programIDsJSON(released)); err != nil {
			return fmt.Errorf("deleting released reservations: %w", err)
		}
		result.ReleasedProgramIDs = released
	}
	derived := removeRulerProgramIDs(plan.DeleteProgramIDs, released)
	if plan.RetractGrace > 0 && len(derived) > 0 {
		protected, err := queryRows(ctx, tx, "grace-protected reservations", `
  SELECT r.program_id FROM reservations r
  JOIN epg_programs p USING (site, program_id)
  JOIN rules ru ON ru.id = r.rule_id AND ru.enabled
  WHERE r.site = ? AND r.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
    AND p.start_at >= ? AND p.start_at < ?
  `, scanProgramID, site, programIDsJSON(derived), plan.Now, plan.Now.Add(plan.RetractGrace))
		if err != nil {
			return err
		}
		result.GraceProtected = len(protected)
		derived = removeRulerProgramIDs(derived, protected)
	}
	switch {
	case len(derived) > plan.MaxDeletesPerPass:
		programs, err := queryRows(ctx, tx, "ruler breaker sample", `
  SELECT s.program_id, s.title FROM program_snapshots s
  WHERE s.site = ? AND s.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
  `, func(rows *sql.Rows) (store.BreakerSampleProgram, error) {
			var p store.BreakerSampleProgram
			err := rows.Scan(&p.ProgramID, &p.Title)
			return p, err
		}, site, programIDsJSON(derived[:min(len(derived), store.BreakerSampleLimit)]))
		if err != nil {
			return err
		}
		sample := store.BreakerSample{Total: len(derived), Programs: programs}
		trippedAt, err := tripCircuitBreaker(ctx, tx, site, plan.BreakerName, plan.MaxDeletesPerPass, sample, &now)
		if err != nil {
			return err
		}
		result.BreakerTripped = true
		result.NewlyTripped = !plan.BreakerTripped
		result.BreakerSample = sample
		result.BreakerTrippedAt = trippedAt
	case plan.BreakerTripped:
		result.BreakerLatched = len(derived) > 0
	case len(derived) > 0:
		deleted, err := tx.ExecContext(ctx, `
  DELETE FROM reservations AS r WHERE r.site = ?
    AND r.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
    AND `+noProgramInvestment, site, programIDsJSON(derived))
		if err != nil {
			return fmt.Errorf("deleting derived reservations: %w", err)
		}
		result.Deleted, err = deleted.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting deleted reservations: %w", err)
		}
	}
	return nil
}

func removeRulerProgramIDs(ids, removed []int64) []int64 {
	if len(removed) == 0 {
		return ids
	}
	set := rulerProgramIDSet(removed)
	return slices.DeleteFunc(slices.Clone(ids), func(id int64) bool {
		_, ok := set[id]
		return ok
	})
}

// PruneEndedSnapshots は指定時刻より前に終了した番組のスナップショットを削除する。
func (s *Reservations) PruneEndedSnapshots(ctx context.Context, before time.Time) (int64, error) {
	var deleted int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
  DELETE FROM program_snapshots WHERE TIMESTAMPADD(MICROSECOND, duration_ms * 1000, start_at) < ?
  `, before)
		if err != nil {
			return fmt.Errorf("deleting ended snapshots: %w", err)
		}
		deleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting pruned snapshots: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

var _ store.RulerPass = (*Reservations)(nil)
