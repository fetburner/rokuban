package cmdetect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// errNoActiveOriginal は、読み取りジョブを積む置き場を決められない（active な原本のコピーが無い）ことを表す。
// 次の reconcile が引き直す。
var errNoActiveOriginal = errors.New("no active original copy to choose a store from")

// CMDetectReconcileWorker は、希望状態に残る CM 検出とロゴ候補のジョブを補う。
// 録画の view は、検出が成功するか現在の試行が失敗すると録画を候補から外す（新しい局ロゴを学習すると再び候補になる）。
// 局の view は候補行が作られた時点で局を候補から外す。これらのため、カーソルを持たない。
// encode reconcile と違い、SQL の後に Go 側で候補を絞り込まない。
// Go 側に絞り込みを足すなら、満杯の窓の先頭に候補が残り続けてカーソルが必要にならないか見直す。
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

// Work は検出とロゴ候補を投入し、対応するジョブの無い候補を失敗にする。
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
	if w.Locator == nil {
		return fmt.Errorf("CM detection reconcile: original locator is nil")
	}
	detectStores, err := w.Locator.OriginalStores(ctx, detectIDs)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: locating originals: %w", err)
	}
	for _, recording := range rows {
		var err error
		if storeName := detectStores[recording.RecordingID]; storeName == "" {
			err = errNoActiveOriginal
		} else {
			_, err = w.Enqueuer.Enqueue(ctx, jobs.CMDetectJobArgs{
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
	candidateStores, err := w.Locator.OriginalStores(ctx, candidateIDs)
	if err != nil {
		return fmt.Errorf("CM detection reconcile: locating originals: %w", err)
	}
	for _, candidate := range candidateRows {
		var err error
		if storeName := candidateStores[candidate.RecordingID]; storeName == "" {
			err = errNoActiveOriginal
		} else {
			_, err = w.Enqueuer.Enqueue(ctx, jobs.CMLogoCandidateJobArgs{
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
