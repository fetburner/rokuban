package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

const (
	defaultCMDetectReconcileInterval = 15 * time.Minute
	cmDetectReconcileTimeout         = 5 * time.Minute
	cmLogoCandidateJobListPageSize   = 10_000
)

// CMDetectReconcileWorker fills the desired CM detection and logo candidate sets.
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

// Work schedules the current desired set and fails candidates without active jobs.
func (w *CMDetectReconcileWorker) Work(ctx context.Context, _ *river.Job[jobs.CMDetectReconcileArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: getting River client: %w", err)
	}
	if err := failOrphanCMLogoCandidates(ctx, client, w.Pool); err != nil {
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
	for _, recording := range rows {
		if _, err := client.Insert(ctx, jobs.CMDetectJobArgs{
			RecordingID: recording.RecordingID, RecordingDurationMs: recording.RecordingDurationMs,
		}, nil); err != nil {
			failed++
			slog.Error("cm_detect_reconcile: failed to enqueue detection", "recording_id", recording.RecordingID, "err", err)
		}
	}
	next := int64(0)
	if int32(len(rows)) >= limit {
		next = rows[len(rows)-1].RecordingID
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
			RecordingDurationMs: candidate.RecordingDurationMs,
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

// failOrphanCMLogoCandidates uses River's documented job-list API to find active
// candidate jobs, then marks only still-current candidate rows that have no match.
func failOrphanCMLogoCandidates(ctx context.Context, client *river.Client[pgx5.Tx], pool *pgxpool.Pool) error {
	candidates, err := sqlcgen.New(pool).ListRunningCMLogoCandidates(ctx)
	if err != nil {
		return fmt.Errorf("listing running CM logo candidates: %w", err)
	}
	active := make(map[cmLogoCandidateKey]struct{})
	params := activeCMLogoCandidateJobListParams(nil)
	for {
		page, err := client.JobList(ctx, params)
		if err != nil {
			return fmt.Errorf("listing active CM logo candidate jobs: %w", err)
		}
		for _, job := range page.Jobs {
			var args jobs.CMLogoCandidateJobArgs
			if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
				return fmt.Errorf("decoding CM logo candidate job %d args: %w", job.ID, err)
			}
			active[cmLogoCandidateKeyFromArgs(args)] = struct{}{}
		}
		if len(page.Jobs) < cmLogoCandidateJobListPageSize {
			break
		}
		if page.LastCursor == nil {
			return fmt.Errorf("listing active CM logo candidate jobs: full page has no cursor")
		}
		params = activeCMLogoCandidateJobListParams(page.LastCursor)
	}

	q := sqlcgen.New(pool)
	for _, candidate := range candidates {
		key := cmLogoCandidateKeyFor(candidate.NetworkID, candidate.ServiceID,
			candidate.RecordingID, candidate.ObservedAreaUpdatedAt)
		if _, ok := active[key]; ok {
			continue
		}
		if _, err := q.FailOrphanCMLogoCandidate(ctx, sqlcgen.FailOrphanCMLogoCandidateParams{
			NetworkID: candidate.NetworkID, ServiceID: candidate.ServiceID,
			RecordingID: &candidate.RecordingID, AreaUpdatedAt: candidate.ObservedAreaUpdatedAt,
			AttemptedAt: candidate.AttemptedAt,
		}); err != nil {
			return fmt.Errorf("failing orphan CM logo candidate for network %d service %d: %w",
				candidate.NetworkID, candidate.ServiceID, err)
		}
	}
	return nil
}

type cmLogoCandidateKey struct {
	networkID            int32
	serviceID            int32
	recordingID          int64
	areaUpdatedAtUnixMic int64
}

func cmLogoCandidateKeyFromArgs(args jobs.CMLogoCandidateJobArgs) cmLogoCandidateKey {
	return cmLogoCandidateKeyFor(args.NetworkID, args.ServiceID, args.RecordingID, args.AreaUpdatedAt)
}

func cmLogoCandidateKeyFor(networkID, serviceID int32, recordingID int64, areaUpdatedAt time.Time) cmLogoCandidateKey {
	return cmLogoCandidateKey{
		networkID: networkID, serviceID: serviceID, recordingID: recordingID,
		areaUpdatedAtUnixMic: areaUpdatedAt.UTC().UnixMicro(),
	}
}

func activeCMLogoCandidateJobListParams(cursor *river.JobListCursor) *river.JobListParams {
	params := river.NewJobListParams().
		Kinds("cm_logo_candidate").
		States(
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateRetryable,
			rivertype.JobStateRunning,
			rivertype.JobStateScheduled,
		).
		First(cmLogoCandidateJobListPageSize)
	if cursor != nil {
		params.After(cursor)
	}
	return params
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
	durationMs, err := sqlcgen.New(pool).GetCMRecordingDuration(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("getting recording %d duration for CM detection: %w", recordingID, err)
	}
	if _, err := client.Insert(ctx, jobs.CMDetectJobArgs{
		RecordingID: recordingID, RecordingDurationMs: durationMs,
	}, nil); err != nil {
		return fmt.Errorf("inserting CM detection for recording %d: %w", recordingID, err)
	}
	return nil
}
