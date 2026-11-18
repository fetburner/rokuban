package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// CMDetectReconcileWorker fills the desired CM detection and logo candidate sets.
// It has no cursor because the CM recording view drops a candidate after detection
// succeeds or its current attempt fails (unless a newer station logo was learned),
// while the station view drops it as soon as a logo-candidate row exists. Unlike
// encode reconcile, it does not filter candidates in Go after the SQL query. If
// Go-side filtering is added, review whether it can leave candidates at the head of
// a full window and therefore require a cursor.
type CMDetectReconcileWorker struct {
	Enqueuer       jobq.Enqueuer
	DetectionStore store.CMDetection
	// Locator は原本のコピーがある置き場を返し、検出ジョブを積む物理キューの置き場を決める。
	Locator       store.OriginalLocator
	RowLimit      int32
	RecoveryGrace time.Duration
}

func cmDetectionTimeout(durationMs int64) time.Duration {
	args := jobs.CMLogoCandidateJobArgs{RecordingDurationMs: durationMs}
	return args.Spec().Timeout(args)
}

// Work schedules the current desired set and fails candidates without active jobs.
func (w *CMDetectReconcileWorker) Work(ctx context.Context, _ jobs.CMDetectReconcileArgs) error {
	if w.Enqueuer == nil {
		return fmt.Errorf("CM detection reconcile: job enqueuer is nil")
	}
	if err := w.DetectionStore.DeleteStaleAttempts(ctx); err != nil {
		return fmt.Errorf("CM detection reconcile: clearing disabled attempts: %w", err)
	}
	if err := w.DetectionStore.FailOrphanLogoCandidates(ctx, w.RecoveryGrace, cmDetectionTimeout); err != nil {
		slog.Warn("cm_detect_reconcile: orphan-candidate recovery had errors", "err", err)
	}
	limit := w.RowLimit
	if limit <= 0 {
		limit = cmDetectRowLimit
	}
	rows, err := w.DetectionStore.ListMissingDetections(ctx, limit)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: listing desired recordings: %w", err)
	}
	failed := 0
	detectIDs := make([]int64, 0, len(rows))
	for _, recording := range rows {
		detectIDs = append(detectIDs, recording.RecordingID)
	}
	detectStores, err := originalStoresOf(ctx, w.Locator, detectIDs)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: %w", err)
	}
	for _, recording := range rows {
		var err error
		if storeName := detectStores[recording.RecordingID]; storeName == "" {
			err = errNoActiveOriginal
		} else {
			err = enqueueJob(ctx, w.Enqueuer, jobs.CMDetectJobArgs{
				Store:       storeName,
				RecordingID: recording.RecordingID, RecordingDurationMs: recording.RecordingDurationMs,
			})
		}
		if err != nil {
			failed++
			slog.Error("cm_detect_reconcile: failed to enqueue detection", "recording_id", recording.RecordingID, "err", err)
		}
	}
	if len(rows) > 0 || failed > 0 {
		slog.Info("cm_detect_reconcile: pass complete", "candidates", len(rows), "enqueue_failures", failed,
			"row_limit", limit)
	}
	candidateRows, err := w.DetectionStore.ListMissingLogoCandidates(ctx, limit)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: listing desired logo candidates: %w", err)
	}
	candidateFailures := 0
	candidateIDs := make([]int64, 0, len(candidateRows))
	for _, candidate := range candidateRows {
		candidateIDs = append(candidateIDs, candidate.RecordingID)
	}
	candidateStores, err := originalStoresOf(ctx, w.Locator, candidateIDs)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: %w", err)
	}
	for _, candidate := range candidateRows {
		var err error
		if storeName := candidateStores[candidate.RecordingID]; storeName == "" {
			err = errNoActiveOriginal
		} else {
			err = enqueueJob(ctx, w.Enqueuer, jobs.CMLogoCandidateJobArgs{
				Store:     storeName,
				NetworkID: candidate.Station.NetworkID, ServiceID: candidate.Station.ServiceID,
				RecordingID: candidate.RecordingID, AreaUpdatedAt: candidate.AreaUpdatedAt,
				RecordingDurationMs: candidate.RecordingDurationMs,
			})
		}
		if err != nil {
			candidateFailures++
			slog.Error("cm_detect_reconcile: failed to enqueue logo candidate",
				"network_id", candidate.Station.NetworkID, "service_id", candidate.Station.ServiceID, "err", err)
		}
	}
	if len(candidateRows) > 0 || candidateFailures > 0 {
		slog.Info("cm_detect_reconcile: logo candidate pass complete",
			"candidates", len(candidateRows), "enqueue_failures", candidateFailures,
			"row_limit", limit)
	}
	return nil
}

// EnqueueCMDetectionIfNeeded shares the periodic pass predicate for ingest's immediate hint.
// storeName は原本を書いた置き場で、検出ジョブの物理キューの置き場になる。
func EnqueueCMDetectionIfNeeded(ctx context.Context, detectionStore store.CMDetection, enqueuer jobq.Enqueuer, recordingID int64, storeName string) error {
	desired, err := detectionStore.IsDetectionDesired(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking desired CM detection for recording %d: %w", recordingID, err)
	}
	if !desired {
		return nil
	}
	durationMs, err := detectionStore.GetRecordingDuration(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("getting recording %d duration for CM detection: %w", recordingID, err)
	}
	if err := enqueueJob(ctx, enqueuer, jobs.CMDetectJobArgs{
		Store:       storeName,
		RecordingID: recordingID, RecordingDurationMs: durationMs,
	}); err != nil {
		return fmt.Errorf("inserting CM detection for recording %d: %w", recordingID, err)
	}
	return nil
}
