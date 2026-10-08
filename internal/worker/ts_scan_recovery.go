package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/tsscan"
)

const (
	tsScanRecoveryStaleAfter    = time.Minute
	tsScanRecoveryMaxJobsPerRun = 100
	tsScanRecoveryReason        = "TS scan process death detected: job advisory lock was not held"
)

const listStaleTSScanJobsQuery = `
SELECT j.id, (j.args->>'recording_id')::bigint, j.attempted_at, j.attempt
FROM river_job AS j
WHERE j.kind = 'ts_scan'
  AND j.state = 'running'
  AND j.args->>'recording_id' IS NOT NULL
  AND j.attempted_at < $1
ORDER BY j.id
LIMIT $2`

const discardRecoveredTSScanJobQuery = `
UPDATE river_job
SET state = 'discarded',
    finalized_at = $2,
    errors = array_append(COALESCE(errors, '{}'::jsonb[]), $3::jsonb),
    metadata = COALESCE(metadata, '{}'::jsonb) || $4::jsonb
WHERE id = $1
  AND kind = 'ts_scan'
  AND state = 'running'`

type staleTSScanJob struct {
	id          int64
	recordingID int64
	attemptedAt time.Time
	attempt     int
}

var recoverStaleTSScanJobsFunc = recoverStaleTSScanJobs

// recoverStaleTSScanJobs replaces scans that River cannot recover because their timeout is unlimited.
func recoverStaleTSScanJobs(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx]) error {
	rows, err := pool.Query(ctx, listStaleTSScanJobsQuery,
		time.Now().UTC().Add(-tsScanRecoveryStaleAfter), tsScanRecoveryMaxJobsPerRun)
	if err != nil {
		return fmt.Errorf("listing stale running TS scan jobs: %w", err)
	}

	return recoverStaleJobCandidates(
		ctx,
		rows,
		"TS scan",
		func(rows pgx5.Rows) (staleTSScanJob, error) {
			var candidate staleTSScanJob
			err := rows.Scan(&candidate.id, &candidate.recordingID, &candidate.attemptedAt, &candidate.attempt)
			return candidate, err
		},
		func(candidate staleTSScanJob) int64 { return candidate.id },
		func(ctx context.Context, id int64) (*jobLock, bool, error) {
			return acquireTSScanJobLock(ctx, pool, id, defaultJobLockTimeout)
		},
		func(ctx context.Context, conn *pgxpool.Conn, candidate staleTSScanJob) error {
			return recoverStaleTSScanJob(ctx, conn, riverClient, candidate)
		},
	)
}

func recoverStaleTSScanJob(
	ctx context.Context,
	conn *pgxpool.Conn,
	riverClient *river.Client[pgx5.Tx],
	candidate staleTSScanJob,
) error {
	inserted, err := replaceStaleRiverJob(
		ctx,
		conn,
		riverClient,
		"ts_scan",
		candidate.id,
		candidate.attempt,
		candidate.attemptedAt,
		tsScanRecoveryReason,
		"ts_scan_recovery",
		discardRecoveredTSScanJobQuery,
		tsscan.ScanArgs{RecordingID: candidate.recordingID},
		nil,
	)
	if err != nil || inserted == nil {
		return err
	}

	newJobID := int64(0)
	if inserted.Job != nil {
		newJobID = inserted.Job.ID
	}
	slog.Info("ts_scan: recovered stale running job",
		"old_job_id", candidate.id,
		"new_job_id", newJobID,
		"new_job_unique_skipped", inserted.UniqueSkippedAsDuplicate,
		"recording_id", candidate.recordingID,
		"attempted_at", candidate.attemptedAt,
	)
	return nil
}
