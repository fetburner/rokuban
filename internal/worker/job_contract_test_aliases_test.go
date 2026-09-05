package worker

import "github.com/fetburner/rokuban/internal/jobs"

// worker パッケージのテストは、契約が internal/jobs へ移る前からこのパッケージに
// 生えていたジョブ名・キュー名でハンドラのフィクスチャを組み立てている。
// 契約の実体を書き直させずに済むよう、型とキュー名定数だけのテスト専用 alias を
// ここに残す（約 250 箇所から参照されており、書き直すより残す方が安い）。
//
// 判定ロジック（qualifyQueueName / physicalQueueName / AllQueueNames /
// RequiresEncodeTools / RequiresSiteBinding / uniqueByQueue / riverQueueNameMaxLen /
// siteBoundQueueNames）の alias は置かない。契約そのものを測るテストは
// internal/jobs 側に移設済みで、worker 側の呼び出し元は jobs.PhysicalQueueName 等を
// 直接呼ぶ。
type IngestJobArgs = jobs.IngestJobArgs
type EpgSyncArgs = jobs.EpgSyncArgs
type TunerSyncArgs = jobs.TunerSyncArgs
type RulerPassArgs = jobs.RulerPassArgs
type ReconcilePassArgs = jobs.ReconcilePassArgs
type RecordSweepArgs = jobs.RecordSweepArgs
type EncodeJobArgs = jobs.EncodeJobArgs
type EncodeEnqueueHintArgs = jobs.EncodeEnqueueHintArgs
type ThumbnailJobArgs = jobs.ThumbnailJobArgs
type EncodeReconcileArgs = jobs.EncodeReconcileArgs
type DeleteReconcileArgs = jobs.DeleteReconcileArgs
type CatalogExportArgs = jobs.CatalogExportArgs
type StorageSyncArgs = jobs.StorageSyncArgs

const (
	ingestQueue      = jobs.IngestQueue
	epgQueue         = jobs.EpgQueue
	rulerQueue       = jobs.RulerQueue
	reconcilerQueue  = jobs.ReconcilerQueue
	recordSweepQueue = jobs.RecordSweepQueue
	encodeQueue      = jobs.EncodeQueue
	thumbnailQueue   = jobs.ThumbnailQueue
	cleanupQueue     = jobs.CleanupQueue
)
