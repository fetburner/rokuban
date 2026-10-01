package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

const (
	defaultCMDetectReconcileInterval = 15 * time.Minute
	cmDetectReconcileTimeout         = 5 * time.Minute
)

// CMDetectReconcileWorker fills desired CM detections and recovers dead running jobs.
type CMDetectReconcileWorker struct {
	river.WorkerDefaults[jobs.CMDetectReconcileArgs]
	Pool                  *pgxpool.Pool
	RowLimit              int32
	resumeAfter           atomic.Int64
	candidateAfterNetwork atomic.Int32
	candidateAfterService atomic.Int32
}

// Timeout returns the cap for candidate scans and River inserts.
func (w *CMDetectReconcileWorker) Timeout(*river.Job[jobs.CMDetectReconcileArgs]) time.Duration {
	return cmDetectReconcileTimeout
}

// Work recovers only stale running jobs, then schedules the current desired set.
func (w *CMDetectReconcileWorker) Work(ctx context.Context, _ *river.Job[jobs.CMDetectReconcileArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: getting River client: %w", err)
	}
	if err := recoverStaleCMDetectJobs(ctx, w.Pool); err != nil {
		slog.Warn("cm_detect_reconcile: stale-job recovery had errors", "err", err)
	}
	if err := recoverStaleCMLogoCandidateJobs(ctx, w.Pool); err != nil {
		slog.Warn("cm_detect_reconcile: stale-candidate recovery had errors", "err", err)
	}
	if err := failOrphanCMLogoCandidates(ctx, w.Pool); err != nil {
		slog.Warn("cm_detect_reconcile: orphan-candidate recovery had errors", "err", err)
	}
	limit := w.RowLimit
	if limit <= 0 {
		limit = cmDetectRowLimit
	}
	rows, err := sqlcgen.New(w.Pool).ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{
		AfterRecordingID: w.resumeAfter.Load(),
		RowLimit:         limit,
	})
	if err != nil {
		return fmt.Errorf("CM detection reconcile: listing desired recordings: %w", err)
	}
	failed := 0
	for _, recordingID := range rows {
		if _, err := client.Insert(ctx, jobs.CMDetectJobArgs{RecordingID: recordingID}, nil); err != nil {
			failed++
			slog.Error("cm_detect_reconcile: failed to enqueue detection", "recording_id", recordingID, "err", err)
		}
	}
	next := int64(0)
	if int32(len(rows)) >= limit {
		next = rows[len(rows)-1]
		slog.Warn("cm_detect_reconcile: candidate window is full", "row_limit", limit, "resume_after", next)
	}
	w.resumeAfter.Store(next)
	if len(rows) > 0 || failed > 0 {
		slog.Info("cm_detect_reconcile: pass complete", "candidates", len(rows), "enqueue_failures", failed, "resume_after", next)
	}
	candidateRows, err := sqlcgen.New(w.Pool).ListMissingCMLogoCandidates(ctx, sqlcgen.ListMissingCMLogoCandidatesParams{
		AfterNetworkID: w.candidateAfterNetwork.Load(),
		AfterServiceID: w.candidateAfterService.Load(),
		RowLimit:       limit,
	})
	if err != nil {
		return fmt.Errorf("CM detection reconcile: listing desired logo candidates: %w", err)
	}
	candidateFailures := 0
	for _, candidate := range candidateRows {
		_, err := client.Insert(ctx, jobs.CMLogoCandidateJobArgs{
			NetworkID: candidate.NetworkID, ServiceID: candidate.ServiceID,
			RecordingID: candidate.RecordingID, AreaUpdatedAt: candidate.AreaUpdatedAt,
		}, nil)
		if err != nil {
			candidateFailures++
			slog.Error("cm_detect_reconcile: failed to enqueue logo candidate",
				"network_id", candidate.NetworkID, "service_id", candidate.ServiceID, "err", err)
		}
	}
	var nextCandidateNetwork, nextCandidateService int32
	if int32(len(candidateRows)) >= limit {
		last := candidateRows[len(candidateRows)-1]
		nextCandidateNetwork, nextCandidateService = last.NetworkID, last.ServiceID
		slog.Warn("cm_detect_reconcile: logo candidate window is full", "row_limit", limit,
			"resume_network_id", nextCandidateNetwork, "resume_service_id", nextCandidateService)
	}
	w.candidateAfterNetwork.Store(nextCandidateNetwork)
	w.candidateAfterService.Store(nextCandidateService)
	if len(candidateRows) > 0 || candidateFailures > 0 {
		slog.Info("cm_detect_reconcile: logo candidate pass complete",
			"candidates", len(candidateRows), "enqueue_failures", candidateFailures,
			"resume_network_id", nextCandidateNetwork, "resume_service_id", nextCandidateService)
	}
	return nil
}

const listStaleCMDetectJobsQuery = `
SELECT id, (args->>'recording_id')::bigint, attempted_at
FROM river_job
WHERE kind = 'cm_detect'
  AND state = 'running'
  AND args->>'recording_id' IS NOT NULL
  AND attempted_at < $1
ORDER BY id
LIMIT $2`

// 試行回数が残っていれば River 自身の再試行(retryable)に戻し、使い切っていれば discarded にする。
// discarded にしたまま reconcile が新しいジョブを積み直すと attempt が 1 に戻り、OOM で
// 落ち続ける録画が failed に届かない。
const recoverStaleCMDetectJobQuery = `
UPDATE river_job
SET state = CASE WHEN attempt < max_attempts THEN 'retryable'::river_job_state ELSE 'discarded'::river_job_state END,
    scheduled_at = CASE WHEN attempt < max_attempts THEN now() ELSE scheduled_at END,
    finalized_at = CASE WHEN attempt < max_attempts THEN NULL ELSE now() END,
    errors = array_append(COALESCE(errors, '{}'::jsonb[]),
      jsonb_build_object('at', now(), 'attempt', attempt,
        'error', 'CM detection process stopped while job was running')::jsonb),
    metadata = COALESCE(metadata, '{}'::jsonb) ||
      jsonb_build_object('cm_detect_recovery', jsonb_build_object('recovered_at', now()))
WHERE id = $1 AND kind = 'cm_detect' AND state = 'running'
RETURNING attempt < max_attempts`

const markStaleCMDetectAttemptQuery = `
UPDATE recording_cm_attempts
SET state = $2, stage = 'stopped', error = 'CM detection process stopped while job was running', attempted_at = now()
WHERE recording_id = $1 AND state = 'running'`

type staleCMDetectJob struct {
	id          int64
	recordingID int64
	attemptedAt time.Time
}

func recoverStaleCMDetectJobs(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, listStaleCMDetectJobsQuery, time.Now().UTC().Add(-cmDetectStaleAfter), 100)
	if err != nil {
		return fmt.Errorf("listing stale CM detection jobs: %w", err)
	}
	candidates := make([]staleCMDetectJob, 0, 100)
	for rows.Next() {
		var candidate staleCMDetectJob
		if err := rows.Scan(&candidate.id, &candidate.recordingID, &candidate.attemptedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scanning stale CM detection job: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading stale CM detection jobs: %w", err)
	}
	rows.Close()

	var errs []error
	for _, candidate := range candidates {
		lock, acquired, err := acquireEncodeJobLock(ctx, pool, candidate.id, defaultJobLockTimeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("locking stale job %d: %w", candidate.id, err))
			continue
		}
		if !acquired {
			continue
		}
		lock.stopHeartbeatLoop()
		retry, err := recoverStaleCMDetectJob(ctx, lock.conn, candidate)
		lock.release()
		if err != nil {
			errs = append(errs, fmt.Errorf("recovering stale job %d: %w", candidate.id, err))
			continue
		}
		slog.Info("cm_detect_reconcile: recovered stale running job",
			"job_id", candidate.id, "recording_id", candidate.recordingID, "attempted_at", candidate.attemptedAt, "retry", retry)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// recoverStaleCMDetectJob は river_job と試行行を同じ tx で更新し、再試行に戻したかを返す。
func recoverStaleCMDetectJob(ctx context.Context, conn *pgxpool.Conn, candidate staleCMDetectJob) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning recovery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var retry bool
	if err := tx.QueryRow(ctx, recoverStaleCMDetectJobQuery, candidate.id).Scan(&retry); err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return false, nil // 別経路で状態が変わった。何もしない
		}
		return false, fmt.Errorf("updating river job: %w", err)
	}
	state := "failed"
	if retry {
		state = "retrying"
	}
	if _, err := tx.Exec(ctx, markStaleCMDetectAttemptQuery, candidate.recordingID, state); err != nil {
		return false, fmt.Errorf("updating attempt state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing recovery: %w", err)
	}
	return retry, nil
}

const listStaleCMLogoCandidateJobsQuery = `
SELECT id,
       (args->>'network_id')::int,
       (args->>'service_id')::int,
       (args->>'area_updated_at')::timestamptz,
       attempted_at
FROM river_job
WHERE kind = 'cm_logo_candidate'
  AND state = 'running'
  AND args->>'network_id' IS NOT NULL
  AND args->>'service_id' IS NOT NULL
  AND args->>'area_updated_at' IS NOT NULL
  AND attempted_at < $1
ORDER BY id
LIMIT $2`

const recoverStaleCMLogoCandidateJobQuery = `
UPDATE river_job
SET state = CASE WHEN attempt < max_attempts THEN 'retryable'::river_job_state ELSE 'discarded'::river_job_state END,
    scheduled_at = CASE WHEN attempt < max_attempts THEN now() ELSE scheduled_at END,
    finalized_at = CASE WHEN attempt < max_attempts THEN NULL ELSE now() END,
    errors = array_append(COALESCE(errors, '{}'::jsonb[]),
      jsonb_build_object('at', now(), 'attempt', attempt,
        'error', 'CM logo candidate process stopped while job was running')::jsonb),
    metadata = COALESCE(metadata, '{}'::jsonb) ||
      jsonb_build_object('cm_logo_candidate_recovery', jsonb_build_object('recovered_at', now()))
WHERE id = $1 AND kind = 'cm_logo_candidate' AND state = 'running'
RETURNING attempt < max_attempts`

const resetStaleCMLogoCandidateQuery = `
DELETE FROM cm_logo_candidates
WHERE network_id = $1
  AND service_id = $2
  AND observed_area_updated_at = $3
  AND state = 'running'`

const failStaleCMLogoCandidateQuery = `
UPDATE cm_logo_candidates
SET state = 'failed', stage = 'stopped',
    error = 'CM logo candidate process stopped while job was running'
WHERE network_id = $1
  AND service_id = $2
  AND observed_area_updated_at = $3
  AND state = 'running'`

// 解析ジョブが生きていない running 行（River が discarded にした・fail() 自体が
// 書けなかった、など）は誰も進めない。River の状態だけを見て回収すると取りこぼすので、
// 候補行の側から「対応する未完了ジョブが無い running」を failed にする。
// running 行を書くのは job が running の間だけなので、この判定は時刻に依らない。
const failOrphanCMLogoCandidatesQuery = `
UPDATE cm_logo_candidates c
SET state = 'failed', stage = 'stopped',
    error = 'CM logo candidate job ended without recording a result'
WHERE c.state = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM river_job j
      WHERE j.kind = 'cm_logo_candidate'
        AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
        AND j.args->>'network_id' = c.network_id::text
        AND j.args->>'service_id' = c.service_id::text
        AND (j.args->>'area_updated_at')::timestamptz = c.observed_area_updated_at
  )`

func failOrphanCMLogoCandidates(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, failOrphanCMLogoCandidatesQuery); err != nil {
		return fmt.Errorf("failing orphan CM logo candidates: %w", err)
	}
	return nil
}

type staleCMLogoCandidateJob struct {
	id            int64
	networkID     int32
	serviceID     int32
	areaUpdatedAt time.Time
	attemptedAt   time.Time
}

func recoverStaleCMLogoCandidateJobs(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, listStaleCMLogoCandidateJobsQuery, time.Now().UTC().Add(-cmDetectStaleAfter), 100)
	if err != nil {
		return fmt.Errorf("listing stale CM logo candidate jobs: %w", err)
	}
	candidates := make([]staleCMLogoCandidateJob, 0, 100)
	for rows.Next() {
		var candidate staleCMLogoCandidateJob
		if err := rows.Scan(&candidate.id, &candidate.networkID, &candidate.serviceID,
			&candidate.areaUpdatedAt, &candidate.attemptedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scanning stale CM logo candidate job: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading stale CM logo candidate jobs: %w", err)
	}
	rows.Close()

	var errs []error
	for _, candidate := range candidates {
		lock, acquired, err := acquireEncodeJobLock(ctx, pool, candidate.id, defaultJobLockTimeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("locking stale candidate job %d: %w", candidate.id, err))
			continue
		}
		if !acquired {
			continue
		}
		lock.stopHeartbeatLoop()
		retry, err := recoverStaleCMLogoCandidateJob(ctx, lock.conn, candidate)
		lock.release()
		if err != nil {
			errs = append(errs, fmt.Errorf("recovering stale candidate job %d: %w", candidate.id, err))
			continue
		}
		slog.Info("cm_detect_reconcile: recovered stale logo candidate job",
			"job_id", candidate.id, "network_id", candidate.networkID,
			"service_id", candidate.serviceID, "attempted_at", candidate.attemptedAt, "retry", retry)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func recoverStaleCMLogoCandidateJob(ctx context.Context, conn *pgxpool.Conn, candidate staleCMLogoCandidateJob) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning candidate recovery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var retry bool
	if err := tx.QueryRow(ctx, recoverStaleCMLogoCandidateJobQuery, candidate.id).Scan(&retry); err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("updating River candidate job: %w", err)
	}
	if retry {
		if _, err := tx.Exec(ctx, resetStaleCMLogoCandidateQuery,
			candidate.networkID, candidate.serviceID, candidate.areaUpdatedAt); err != nil {
			return false, fmt.Errorf("resetting stale candidate row: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, failStaleCMLogoCandidateQuery,
			candidate.networkID, candidate.serviceID, candidate.areaUpdatedAt); err != nil {
			return false, fmt.Errorf("failing stale candidate row: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing candidate recovery: %w", err)
	}
	return retry, nil
}

// EnqueueCMDetectionIfNeeded shares the periodic pass predicate for ingest's immediate hint.
func EnqueueCMDetectionIfNeeded(ctx context.Context, pool *pgxpool.Pool, client *river.Client[pgx5.Tx], recordingID int64) error {
	if client == nil {
		return nil
	}
	desired, err := sqlcgen.New(pool).IsCMDetectionDesired(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking desired CM detection for recording %d: %w", recordingID, err)
	}
	if !desired {
		return nil
	}
	if _, err := client.Insert(ctx, jobs.CMDetectJobArgs{RecordingID: recordingID}, nil); err != nil {
		return fmt.Errorf("inserting CM detection for recording %d: %w", recordingID, err)
	}
	return nil
}
