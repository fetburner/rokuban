package jobs

import (
	"time"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobq"
)

const (
	defaultJobTimeout            = time.Minute
	ingestJobTimeout             = 5 * time.Minute
	epgSyncJobTimeout            = 10 * time.Minute
	tunerSyncJobTimeout          = time.Minute
	rulerPassJobTimeout          = 5 * time.Minute
	reconcilePassJobTimeout      = 10 * time.Minute
	recordSweepJobTimeout        = 10 * time.Minute
	thumbnailJobTimeout          = 5 * time.Minute
	seekTilesJobTimeout          = 15 * time.Minute
	thumbnailReconcileJobTimeout = 5 * time.Minute
	cmDetectReconcileJobTimeout  = 5 * time.Minute
	encodeReconcileJobTimeout    = 5 * time.Minute
	deleteReconcileJobTimeout    = 10 * time.Minute
	catalogExportJobTimeout      = 5 * time.Minute
	storageSyncJobTimeout        = time.Minute
)

// EncodeUnknownDurationTimeout is the execution limit for an encode job without a saved
// duration-based deadline, including older queued arguments.
const EncodeUnknownDurationTimeout = 12 * time.Hour

func fixedTimeout(timeout time.Duration) func(jobq.Args) time.Duration {
	return func(jobq.Args) time.Duration { return timeout }
}

func spec(queue string, timeout func(jobq.Args) time.Duration, maxAttempts int, coalesce bool, priority int) jobq.Spec {
	if priority == 0 {
		priority = 1
	}
	return jobq.Spec{
		Queue:       queue,
		Timeout:     timeout,
		MaxAttempts: maxAttempts,
		Coalesce:    coalesce,
		Priority:    priority,
	}
}

func cmDetectionTimeout(args jobq.Args) time.Duration {
	var durationMs int64
	switch args := args.(type) {
	case CMDetectJobArgs:
		durationMs = args.RecordingDurationMs
	case CMLogoCandidateJobArgs:
		durationMs = args.RecordingDurationMs
	default:
		return 0
	}
	minimum := 30 * time.Minute
	if durationMs <= 0 || durationMs > int64((time.Duration(1<<63-1)/2)/time.Millisecond) {
		return minimum
	}
	timeout := 2 * time.Duration(durationMs) * time.Millisecond
	if timeout < minimum {
		return minimum
	}
	return timeout
}

// Spec は ingest の論理キュー、有限の 5 分締切、合流ヒントを返す。
func (a IngestJobArgs) Spec() jobq.Spec {
	return spec(IngestQueue, fixedTimeout(ingestJobTimeout), 0, true, 1)
}

// Spec は EPG 同期の論理キュー、10 分締切、合流ヒントを返す。
func (a EpgSyncArgs) Spec() jobq.Spec {
	return spec(EpgQueue, fixedTimeout(epgSyncJobTimeout), 0, true, 1)
}

// Spec はチューナー同期の論理キュー、1 分締切、合流ヒントを返す。
func (a TunerSyncArgs) Spec() jobq.Spec {
	return spec(EpgQueue, fixedTimeout(tunerSyncJobTimeout), 0, true, 1)
}

// Spec は ruler パスの論理キュー、5 分締切、合流ヒントを返す。
// Coalesce は排他を保証しない。
func (a RulerPassArgs) Spec() jobq.Spec {
	return spec(RulerQueue, fixedTimeout(rulerPassJobTimeout), 0, true, 1)
}

// Spec は reconciler パスの論理キュー、10 分締切、合流ヒントを返す。
// Queue は論理名のままにし、River 互換の InsertOpts では site を修飾する。
func (a ReconcilePassArgs) Spec() jobq.Spec {
	return spec(ReconcilerQueue, fixedTimeout(reconcilePassJobTimeout), 0, true, 1)
}

// Spec は record sweep の論理キュー、10 分締切、合流ヒントを返す。
func (a RecordSweepArgs) Spec() jobq.Spec {
	return spec(RecordSweepQueue, fixedTimeout(recordSweepJobTimeout), 0, true, 1)
}

// Spec は encode の論理キュー、保存済み締切または既定の 12 時間締切、26 回の
// adapter 試行上限、合流ヒントを返す。
func (a EncodeJobArgs) Spec() jobq.Spec {
	return spec(EncodeQueue, func(raw jobq.Args) time.Duration {
		args, ok := raw.(EncodeJobArgs)
		if !ok {
			return 0
		}
		if args.Timeout > 0 {
			return args.Timeout
		}
		return EncodeUnknownDurationTimeout
	}, 26, true, 1)
}

// Spec は thumbnail の論理キュー、5 分締切、合流ヒントを返す。
func (a ThumbnailJobArgs) Spec() jobq.Spec {
	return spec(ThumbnailQueue, fixedTimeout(thumbnailJobTimeout), 0, true, 1)
}

// Spec は seek tiles の論理キュー、15 分締切、優先度 4、合流ヒントを返す。
func (a SeekTilesJobArgs) Spec() jobq.Spec {
	return spec(ThumbnailQueue, fixedTimeout(seekTilesJobTimeout), 0, true, 4)
}

// Spec は thumbnail reconcile の論理キュー、5 分締切、合流ヒントを返す。
func (a ThumbnailReconcileArgs) Spec() jobq.Spec {
	return spec(ThumbnailQueue, fixedTimeout(thumbnailReconcileJobTimeout), 0, true, 1)
}

// Spec は CM 検出の論理キュー、録画尺に応じた締切、10 回の adapter 試行上限、
// 合流ヒントを返す。
func (a CMDetectJobArgs) Spec() jobq.Spec {
	return spec(CMDetectQueue, cmDetectionTimeout, 10, true, 1)
}

// Spec は CM 検出 reconcile の論理キュー、5 分締切、合流ヒントを返す。
func (a CMDetectReconcileArgs) Spec() jobq.Spec {
	return spec(CMDetectQueue, fixedTimeout(cmDetectReconcileJobTimeout), 0, true, 1)
}

// Spec は CM ロゴ候補の論理キュー、録画尺に応じた締切、1 回の adapter 試行上限、
// 合流ヒントを返す。
func (a CMLogoCandidateJobArgs) Spec() jobq.Spec {
	return spec(CMDetectQueue, cmDetectionTimeout, 1, true, 1)
}

// Spec は分類ルール reconcile の論理キュー、River 既定と同じ 1 分締切を返す。
// Coalesce は無効にし、編集中に投入された各変更を残す。
func (a LabelRuleReconcileArgs) Spec() jobq.Spec {
	return spec(CleanupQueue, fixedTimeout(defaultJobTimeout), 0, false, 1)
}

// Spec は encode reconcile の論理キュー、5 分締切、合流ヒントを返す。
func (a EncodeReconcileArgs) Spec() jobq.Spec {
	return spec(EncodeQueue, fixedTimeout(encodeReconcileJobTimeout), 0, true, 1)
}

// Spec は利用者が要求した encode rebuild の論理キュー、River 既定と同じ 1 分締切、
// 合流ヒントを返す。
func (a EncodeRebuildArgs) Spec() jobq.Spec {
	return spec(EncodeQueue, fixedTimeout(defaultJobTimeout), 0, true, 1)
}

// Spec は delete reconcile の論理キュー、10 分締切、合流ヒントを返す。
func (a DeleteReconcileArgs) Spec() jobq.Spec {
	return spec(CleanupQueue, fixedTimeout(deleteReconcileJobTimeout), 0, true, 1)
}

// Spec は catalog export の論理キュー、5 分締切、合流ヒントを返す。
func (a CatalogExportArgs) Spec() jobq.Spec {
	return spec(CleanupQueue, fixedTimeout(catalogExportJobTimeout), 0, true, 1)
}

// Spec は storage sync の論理キュー、1 分締切、合流ヒントを返す。
func (a StorageSyncArgs) Spec() jobq.Spec {
	return spec(StorageQueue, fixedTimeout(storageSyncJobTimeout), 0, true, 1)
}

// RiverInsertOpts projects a Spec into the legacy River insert contract.
// physicalQueue must be produced by PhysicalQueueName so enqueue and subscription use the same rule.
func RiverInsertOpts(spec jobq.Spec, physicalQueue string) river.InsertOpts {
	opts := river.InsertOpts{
		Queue:       physicalQueue,
		MaxAttempts: spec.MaxAttempts,
		Priority:    spec.Priority,
	}
	if spec.Coalesce {
		opts.UniqueOpts = river.UniqueOpts{
			ByArgs:  true,
			ByQueue: true,
			ByState: pendingJobStates,
		}
	}
	return opts
}
