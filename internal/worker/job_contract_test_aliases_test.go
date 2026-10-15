package worker

import "github.com/fetburner/rokuban/internal/jobs"

// worker パッケージのテストは、契約が internal/jobs へ移る前からこのパッケージに
// 生えていたジョブ型名でハンドラのフィクスチャを組み立てている。契約の実体を
// 書き直さずに済むよう、型だけのテスト専用 alias をここに残す。
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
type ThumbnailJobArgs = jobs.ThumbnailJobArgs
type SeekTilesJobArgs = jobs.SeekTilesJobArgs
type ThumbnailReconcileArgs = jobs.ThumbnailReconcileArgs
type EncodeReconcileArgs = jobs.EncodeReconcileArgs
type DeleteReconcileArgs = jobs.DeleteReconcileArgs
type CatalogExportArgs = jobs.CatalogExportArgs
type StorageSyncArgs = jobs.StorageSyncArgs
