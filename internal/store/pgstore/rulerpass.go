package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// ListEnabled は有効なルールを優先度降順、ID 昇順で返す。
func (s *Reservations) ListEnabled(ctx context.Context) ([]store.Rule, error) {
	rows, err := sqlcgen.New(s.pool).ListEnabledRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing enabled rules: %w", err)
	}
	rules := make([]store.Rule, 0, len(rows))
	for _, row := range rows {
		rules = append(rules, ruleFromSQL(row))
	}
	return rules, nil
}

// ListProgramIDReuses は終了済みスナップショットと射影を指定時刻で比べる。
func (s *Reservations) ListProgramIDReuses(ctx context.Context, site string, now time.Time) ([]store.RulerProgramIDReuse, error) {
	rows, err := sqlcgen.New(s.pool).ListProgramIDReusesBySite(ctx, sqlcgen.ListProgramIDReusesBySiteParams{Site: site, Now: now})
	if err != nil {
		return nil, fmt.Errorf("listing program_id reuses for site %q: %w", site, err)
	}
	result := make([]store.RulerProgramIDReuse, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.RulerProgramIDReuse{
			ProgramID: row.ProgramID, SnapshotStartAt: row.SnapshotStartAt, ProjectionStartAt: row.ProjectionStartAt,
		})
	}
	return result, nil
}

// ListProgramIntents は site の番組意図を返す。
func (s *Reservations) ListProgramIntents(ctx context.Context, site string) ([]store.RulerProgramIntent, error) {
	rows, err := sqlcgen.New(s.pool).ListProgramIntentActionsBySite(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing program intents for site %q: %w", site, err)
	}
	result := make([]store.RulerProgramIntent, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.RulerProgramIntent{ProgramID: row.ProgramID, Action: row.Action})
	}
	return result, nil
}

// ListInvestmentProgramIDs は record 意図または上書きを持つ番組 ID を返す。
func (s *Reservations) ListInvestmentProgramIDs(ctx context.Context, site string) ([]int64, error) {
	ids, err := sqlcgen.New(s.pool).ListProgramInvestmentProgramIDsBySite(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing program investments for site %q: %w", site, err)
	}
	return ids, nil
}

// ListFulfilledProgramIDs は原本の取り込みが完了した放送イベントの番組 ID を返す。
func (s *Reservations) ListFulfilledProgramIDs(ctx context.Context, site string) ([]int64, error) {
	ids, err := sqlcgen.New(s.pool).ListFulfilledProgramIDsBySite(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing fulfilled programs for site %q: %w", site, err)
	}
	return ids, nil
}

// ListReservationProgramIDs は site の既存予約の番組 ID を返す。
func (s *Reservations) ListReservationProgramIDs(ctx context.Context, site string) ([]int64, error) {
	ids, err := sqlcgen.New(s.pool).ListReservationProgramIDsBySite(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing reservations for site %q: %w", site, err)
	}
	return ids, nil
}

// ListProjectedProgramIDs は指定した番組 ID のうち現在の EPG 射影にあるものを返す。
func (s *Reservations) ListProjectedProgramIDs(ctx context.Context, site string, programIDs []int64) ([]int64, error) {
	if len(programIDs) == 0 {
		return nil, nil
	}
	ids, err := sqlcgen.New(s.pool).ListEpgProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListEpgProgramIDsBySiteAndProgramIDsParams{
		Site: site, ProgramIds: programIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("listing projected programs for site %q: %w", site, err)
	}
	return ids, nil
}

type rulerDedupeInput struct {
	ProgramID int64 `json:"program_id"`
	RuleID    int64 `json:"rule_id"`
}

// evaluateRulerDedupeSQL は候補番組ごとに、同じルールで録画済みの番組を 1 文で探す。
// 判定条件・自己除外・tie-break は docs/recording/ruler.md §3.1「重複排除」にある。
// **`deleted_at IS NULL` を足すのは契約違反**（tombstone も重複排除の根拠に残す）。
//
// similarity() は索引に乗らない。必要になったら次の手順で前段フィルタを足す。
//
//  1. 有効な dedupe ルールの最小閾値を Go 側で求め、
//     set_config('pg_trgm.similarity_threshold', $n, true) で tx ローカルに設定する
//  2. JOIN に rec.title % c.title を前段フィルタとして足す（最小閾値なので
//     全ルールの条件の上位集合になり、取りこぼさない）
//  3. 本判定は similarity() >= c.dedupe_threshold のまま残す
const evaluateRulerDedupeSQL = `
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($2::jsonb) AS d(
        program_id bigint,
        rule_id    bigint
    )
),
candidate AS (
    SELECT d.program_id,
           d.rule_id,
           p.name AS title,
           p.network_id,
           p.service_id,
           p.event_id,
           ru.dedupe_threshold,
           ru.dedupe_window
    FROM input d
    JOIN rules ru       ON ru.id = d.rule_id AND ru.dedupe_enabled
    JOIN epg_programs p ON p.site = $1 AND p.program_id = d.program_id
)
SELECT DISTINCT ON (c.program_id)
       c.program_id,
       rec.id AS recording_id,
       similarity(rec.title, c.title) AS similarity
FROM candidate c
JOIN recordings rec
  ON  rec.rule_id = c.rule_id
  AND rec.status = 'finished'
  AND (rec.network_id, rec.service_id, rec.event_id)
      IS DISTINCT FROM (c.network_id, c.service_id, c.event_id)
  AND (c.dedupe_window IS NULL
       OR rec.program_start_at >= $3::timestamptz - c.dedupe_window)
  AND similarity(rec.title, c.title) >= c.dedupe_threshold
ORDER BY c.program_id, similarity(rec.title, c.title) DESC, rec.id ASC
`

// EvaluateDedupe は候補ごとに最も類似した完了済み録画を返す。
func (s *Reservations) EvaluateDedupe(ctx context.Context, site string, candidates []store.RulerDedupeCandidate, now time.Time) (map[int64]store.RulerDedupeMatch, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	input := make([]rulerDedupeInput, 0, len(candidates))
	for _, candidate := range candidates {
		input = append(input, rulerDedupeInput{ProgramID: candidate.ProgramID, RuleID: candidate.RuleID})
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshalling dedupe candidates: %w", err)
	}
	rows, err := s.pool.Query(ctx, evaluateRulerDedupeSQL, site, payload, now)
	if err != nil {
		return nil, fmt.Errorf("evaluating dedupe for site %q: %w", site, err)
	}
	defer rows.Close()
	matches := make(map[int64]store.RulerDedupeMatch)
	for rows.Next() {
		var programID int64
		var match store.RulerDedupeMatch
		if err := rows.Scan(&programID, &match.RecordingID, &match.Similarity); err != nil {
			return nil, fmt.Errorf("scanning dedupe match: %w", err)
		}
		matches[programID] = match
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading dedupe matches: %w", err)
	}
	return matches, nil
}

type rulerReservationInput struct {
	ProgramID             int64           `json:"program_id"`
	RuleID                *int64          `json:"rule_id"`
	Base                  json.RawMessage `json:"base"`
	DedupMatchRecordingID *int64          `json:"dedup_match_recording_id"`
	DedupSimilarity       *float32        `json:"dedup_similarity"`
}

const upsertRulerReservationsSQL = `
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($2::jsonb) AS d(
        program_id bigint,
        rule_id bigint,
        base jsonb,
        dedup_match_recording_id bigint,
        dedup_similarity real
    )
),
resolved AS (
    SELECT d.program_id,
           d.rule_id,
           CASE WHEN d.rule_id IS NOT NULL THEN d.base ELSE r.base END AS base,
           CASE WHEN d.rule_id IS NOT NULL THEN d.dedup_match_recording_id ELSE r.dedup_match_recording_id END AS dedup_match_recording_id,
           CASE WHEN d.rule_id IS NOT NULL THEN d.dedup_similarity ELSE r.dedup_similarity END AS dedup_similarity
    FROM input d
    LEFT JOIN reservations r ON r.site = $1 AND r.program_id = d.program_id
)
INSERT INTO reservations (
    site, program_id, rule_id, base,
    dedup_match_recording_id, dedup_similarity
)
SELECT $1, program_id, rule_id, base,
       dedup_match_recording_id, dedup_similarity
FROM resolved
ON CONFLICT (site, program_id) DO UPDATE SET
    rule_id                  = EXCLUDED.rule_id,
    base                     = EXCLUDED.base,
    dedup_match_recording_id = EXCLUDED.dedup_match_recording_id,
    dedup_similarity         = EXCLUDED.dedup_similarity,
    updated_at               = now()
WHERE reservations.rule_id                  IS DISTINCT FROM EXCLUDED.rule_id
   OR reservations.base                     IS DISTINCT FROM EXCLUDED.base
   OR reservations.dedup_match_recording_id IS DISTINCT FROM EXCLUDED.dedup_match_recording_id
   OR reservations.dedup_similarity         IS DISTINCT FROM EXCLUDED.dedup_similarity
RETURNING program_id
`

// ApplyPass は ruler の計画を 1 tx で反映し、削除を保護する判断も同じ tx に入れる。
func (s *Reservations) ApplyPass(ctx context.Context, site string, plan store.RulerPassPlan) (store.RulerPassResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.RulerPassResult{}, fmt.Errorf("beginning ruler pass for site %q: %w", site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := upsertRulerSnapshots(ctx, q, site, plan.SnapshotProgramIDs); err != nil {
		return store.RulerPassResult{}, err
	}
	result, err := upsertRulerReservations(ctx, tx, q, site, plan)
	if err != nil {
		return store.RulerPassResult{}, err
	}
	result.FulfilledDeleted, err = deleteFulfilledRulerReservations(ctx, q, site, plan.FulfilledProgramIDs)
	if err != nil {
		return store.RulerPassResult{}, err
	}
	deleteResult, err := applyRulerDeletes(ctx, q, site, plan)
	if err != nil {
		return store.RulerPassResult{}, err
	}
	result.ReleasedProgramIDs = deleteResult.ReleasedProgramIDs
	result.Deleted = deleteResult.Deleted
	result.GraceProtected = deleteResult.GraceProtected
	result.BreakerLatched = deleteResult.BreakerLatched
	result.BreakerTripped = deleteResult.BreakerTripped
	result.NewlyTripped = deleteResult.NewlyTripped
	result.BreakerTrippedAt = deleteResult.BreakerTrippedAt
	result.BreakerSample = deleteResult.BreakerSample

	if err := tx.Commit(ctx); err != nil {
		return store.RulerPassResult{}, fmt.Errorf("committing ruler pass for site %q: %w", site, mapDatabaseError(err))
	}
	return result, nil
}

func upsertRulerSnapshots(ctx context.Context, q *sqlcgen.Queries, site string, programIDs []int64) error {
	if len(programIDs) == 0 {
		return nil
	}
	if _, err := q.UpsertProgramSnapshotsFromProjection(ctx, sqlcgen.UpsertProgramSnapshotsFromProjectionParams{
		Site: site, ProgramIds: programIDs,
	}); err != nil {
		return fmt.Errorf("upserting program snapshots for site %q: %w", site, mapDatabaseError(err))
	}
	return nil
}

func upsertRulerReservations(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, site string, plan store.RulerPassPlan) (store.RulerPassResult, error) {
	result := store.RulerPassResult{}
	if len(plan.Reservations) == 0 {
		return result, nil
	}
	programIDs := make([]int64, 0, len(plan.Reservations))
	for _, row := range plan.Reservations {
		programIDs = append(programIDs, row.ProgramID)
	}
	materializableIDs, err := q.ListProgramSnapshotProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListProgramSnapshotProgramIDsBySiteAndProgramIDsParams{
		Site: site, ProgramIds: programIDs,
	})
	if err != nil {
		return result, fmt.Errorf("checking program snapshots for site %q: %w", site, err)
	}
	materializable := make(map[int64]struct{}, len(materializableIDs))
	for _, id := range materializableIDs {
		materializable[id] = struct{}{}
	}
	input := make([]rulerReservationInput, 0, len(plan.Reservations))
	for _, row := range plan.Reservations {
		if _, ok := materializable[row.ProgramID]; !ok {
			result.UnmaterializedProgramIDs = append(result.UnmaterializedProgramIDs, row.ProgramID)
			continue
		}
		input = append(input, rulerReservationInput{
			ProgramID: row.ProgramID, RuleID: row.RuleID, Base: row.Base,
			DedupMatchRecordingID: row.DedupMatchRecordingID, DedupSimilarity: row.DedupSimilarity,
		})
	}
	if len(input) == 0 {
		return result, nil
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return result, fmt.Errorf("marshalling ruler reservation inputs: %w", err)
	}
	rows, err := tx.Query(ctx, upsertRulerReservationsSQL, site, payload)
	if err != nil {
		return result, fmt.Errorf("upserting reservations for site %q: %w", site, mapDatabaseError(err))
	}
	defer rows.Close()
	existing := make(map[int64]struct{}, len(plan.ExistingProgramIDs))
	for _, id := range plan.ExistingProgramIDs {
		existing[id] = struct{}{}
	}
	for rows.Next() {
		var programID int64
		if err := rows.Scan(&programID); err != nil {
			return result, fmt.Errorf("scanning upserted reservation: %w", err)
		}
		if _, ok := existing[programID]; ok {
			result.Updated++
		} else {
			result.Created++
		}
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("reading upserted reservations: %w", err)
	}
	return result, nil
}

func deleteFulfilledRulerReservations(ctx context.Context, q *sqlcgen.Queries, site string, programIDs []int64) (int64, error) {
	if len(programIDs) == 0 {
		return 0, nil
	}
	deleted, err := q.DeleteFulfilledReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteFulfilledReservationsBySiteAndProgramIDsParams{
		Site: site, ProgramIds: programIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("deleting fulfilled reservations for site %q: %w", site, mapDatabaseError(err))
	}
	return int64(len(deleted)), nil
}

func applyRulerDeletes(ctx context.Context, q *sqlcgen.Queries, site string, plan store.RulerPassPlan) (store.RulerPassResult, error) {
	result := store.RulerPassResult{}
	if len(plan.DeleteProgramIDs) == 0 {
		return result, nil
	}
	released, err := q.DeleteReleasedReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReleasedReservationsBySiteAndProgramIDsParams{
		Site: site, ProgramIds: plan.DeleteProgramIDs,
	})
	if err != nil {
		return result, fmt.Errorf("deleting released reservations for site %q: %w", site, mapDatabaseError(err))
	}
	result.ReleasedProgramIDs = released
	derivedDeletes := removeProgramIDs(plan.DeleteProgramIDs, released)
	if plan.RetractGrace > 0 && len(derivedDeletes) > 0 {
		protected, err := q.ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListRetractGraceProtectedProgramIDsBySiteAndProgramIDsParams{
			Site: site, ProgramIds: derivedDeletes, Now: plan.Now, GraceUntil: plan.Now.Add(plan.RetractGrace),
		})
		if err != nil {
			return result, fmt.Errorf("checking retract grace for site %q: %w", site, err)
		}
		result.GraceProtected = len(protected)
		derivedDeletes = removeProgramIDs(derivedDeletes, protected)
	}
	if err := applyRulerDeleteGuard(ctx, q, site, plan, derivedDeletes, &result); err != nil {
		return result, err
	}
	return result, nil
}

func applyRulerDeleteGuard(ctx context.Context, q *sqlcgen.Queries, site string, plan store.RulerPassPlan, derivedDeletes []int64, result *store.RulerPassResult) error {
	switch {
	case len(derivedDeletes) > plan.MaxDeletesPerPass:
		sample, err := reservationDeleteSample(ctx, q, site, derivedDeletes)
		if err != nil {
			return fmt.Errorf("building breaker sample for site %q: %w", site, err)
		}
		trippedAt, err := tripCircuitBreaker(ctx, q, site, plan.BreakerName, plan.MaxDeletesPerPass, sample)
		if err != nil {
			return fmt.Errorf("tripping ruler delete breaker for site %q: %w", site, err)
		}
		result.BreakerTripped = true
		result.NewlyTripped = !plan.BreakerTripped
		result.BreakerTrippedAt = trippedAt
		result.BreakerSample = sample
	case plan.BreakerTripped:
		result.BreakerLatched = len(derivedDeletes) > 0
	case len(derivedDeletes) > 0:
		deleted, err := q.DeleteReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReservationsBySiteAndProgramIDsParams{
			Site: site, ProgramIds: derivedDeletes,
		})
		if err != nil {
			return fmt.Errorf("deleting stale reservations for site %q: %w", site, mapDatabaseError(err))
		}
		result.Deleted = deleted
	}
	return nil
}

func reservationDeleteSample(ctx context.Context, q *sqlcgen.Queries, site string, programIDs []int64) (store.BreakerSample, error) {
	sampleIDs := programIDs
	if len(sampleIDs) > store.BreakerSampleLimit {
		sampleIDs = sampleIDs[:store.BreakerSampleLimit]
	}
	titles, err := q.ListReservationTitlesBySiteAndProgramIDs(ctx, sqlcgen.ListReservationTitlesBySiteAndProgramIDsParams{
		Site: site, ProgramIds: sampleIDs,
	})
	if err != nil {
		return store.BreakerSample{}, fmt.Errorf("listing reservation titles: %w", err)
	}
	programs := make([]store.BreakerSampleProgram, 0, len(titles))
	for _, title := range titles {
		programs = append(programs, store.BreakerSampleProgram{ProgramID: title.ProgramID, Title: title.Title})
	}
	return store.BreakerSample{Total: len(programIDs), Programs: programs}, nil
}

func removeProgramIDs(programIDs, removed []int64) []int64 {
	if len(removed) == 0 {
		return programIDs
	}
	removedSet := make(map[int64]struct{}, len(removed))
	for _, id := range removed {
		removedSet[id] = struct{}{}
	}
	result := make([]int64, 0, len(programIDs))
	for _, id := range programIDs {
		if _, ok := removedSet[id]; !ok {
			result = append(result, id)
		}
	}
	return result
}

// PruneEndedSnapshots は指定 cutoff より前に終了した番組スナップショットを削除する。
func (s *Reservations) PruneEndedSnapshots(ctx context.Context, before time.Time) (int64, error) {
	deleted, err := sqlcgen.New(s.pool).DeleteEndedProgramSnapshots(ctx, before)
	if err != nil {
		return 0, fmt.Errorf("pruning ended program snapshots: %w", mapDatabaseError(err))
	}
	return deleted, nil
}

var _ store.RulerPass = (*Reservations)(nil)
