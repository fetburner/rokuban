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
	Pool        *pgxpool.Pool
	RowLimit    int32
	resumeAfter atomic.Int64
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

const discardStaleCMDetectJobQuery = `
UPDATE river_job
SET state = 'discarded',
    finalized_at = now(),
    errors = array_append(COALESCE(errors, '{}'::jsonb[]),
      jsonb_build_object('at', now(), 'attempt', attempt,
        'error', 'CM detection process stopped while job was running')::jsonb),
    metadata = COALESCE(metadata, '{}'::jsonb) ||
      jsonb_build_object('cm_detect_recovery', jsonb_build_object('recovered_at', now()))
WHERE id = $1 AND kind = 'cm_detect' AND state = 'running'`

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
		_, err = lock.conn.Exec(ctx, discardStaleCMDetectJobQuery, candidate.id)
		lock.release()
		if err != nil {
			errs = append(errs, fmt.Errorf("discarding stale job %d: %w", candidate.id, err))
			continue
		}
		slog.Info("cm_detect_reconcile: recovered stale running job",
			"job_id", candidate.id, "recording_id", candidate.recordingID, "attempted_at", candidate.attemptedAt)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
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
