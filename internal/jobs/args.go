package jobs

import (
	"github.com/riverqueue/river"
)

// IngestJobArgs は ingest ジョブの引数。mirakc サイトと record ID を指定する。
type IngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

// Kind は River ジョブの種別名を返す。
func (IngestJobArgs) Kind() string { return "ingest" }

// InsertOpts は ingest キューへ投入するための River 挿入オプションを返す。
func (a IngestJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: PhysicalQueueName(IngestQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// EpgSyncArgs は EPG 全量同期ジョブの引数。
type EpgSyncArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (EpgSyncArgs) Kind() string { return "epg_sync" }

// InsertOpts は EPG キューへ投入するための River 挿入オプションを返す。
func (a EpgSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: PhysicalQueueName(EpgQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// TunerSyncArgs はチューナー射影ジョブの引数。
type TunerSyncArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (TunerSyncArgs) Kind() string { return "tuner_sync" }

// InsertOpts は EPG キューへ投入するための River 挿入オプションを返す。
func (a TunerSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: PhysicalQueueName(EpgQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// RulerPassArgs は ruler の 1 パスジョブの引数。
type RulerPassArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (RulerPassArgs) Kind() string { return "ruler_pass" }

// InsertOpts は ruler キューへ投入するための River 挿入オプションを返す。
func (RulerPassArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: RulerQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// ReconcilePassArgs は reconciler の 1 パス突き合わせジョブの引数。
type ReconcilePassArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (ReconcilePassArgs) Kind() string { return "reconcile_pass" }

// InsertOpts は site 修飾済みの reconciler キューへ投入するための River 挿入
// オプションを返す。
func (a ReconcilePassArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: PhysicalQueueName(ReconcilerQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// RecordSweepArgs は watcher の定期全量突き合わせジョブの引数。
type RecordSweepArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (RecordSweepArgs) Kind() string { return "record_sweep" }

// InsertOpts は site 修飾済みの watcher キューへ投入するための River 挿入
// オプションを返す。
func (a RecordSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: PhysicalQueueName(RecordSweepQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// EncodeJobArgs は encode ジョブの引数。recording とプロファイルを指定する。
type EncodeJobArgs struct {
	RecordingID int64  `json:"recording_id"`
	Profile     string `json:"profile"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeJobArgs) Kind() string { return "encode" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
func (EncodeJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: EncodeQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// EncodeEnqueueHintArgs は事後追加されたエンコードプロファイルを反映する
// ヒントジョブの引数。
type EncodeEnqueueHintArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeEnqueueHintArgs) Kind() string { return "encode_enqueue_hint" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
func (EncodeEnqueueHintArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: EncodeQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// ThumbnailJobArgs は thumbnail ジョブの引数。
type ThumbnailJobArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind は River ジョブの種別名を返す。
func (ThumbnailJobArgs) Kind() string { return "thumbnail" }

// InsertOpts は thumbnail キューへ投入するための River 挿入オプションを返す。
func (ThumbnailJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: ThumbnailQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// EncodeReconcileArgs は encode の desired−observed 定期 reconcile ジョブの引数。
type EncodeReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (EncodeReconcileArgs) Kind() string { return "encode_reconcile" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
func (EncodeReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: EncodeQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// DeleteReconcileArgs は削除 reconcile ジョブの引数。
type DeleteReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (DeleteReconcileArgs) Kind() string { return "delete_reconcile" }

// InsertOpts は cleanup キューへ投入するための River 挿入オプションを返す。
func (DeleteReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: CleanupQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// CatalogExportArgs は catalog エクスポートジョブの引数。
type CatalogExportArgs struct {
	Keep int `json:"keep,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (CatalogExportArgs) Kind() string { return "catalog_export" }

// InsertOpts は cleanup キューへ投入するための River 挿入オプションを返す。
func (CatalogExportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: CleanupQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// StorageSyncArgs はストレージ観測ジョブの引数。
type StorageSyncArgs struct{}

// Kind は River ジョブの種別名を返す。
func (StorageSyncArgs) Kind() string { return "storage_sync" }

// InsertOpts は storage キューへ投入するための River 挿入オプションを返す。
func (StorageSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: StorageQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: UniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// Compile-time assertion that every contract type implements River's job argument
// interface. Keeping these assertions next to the contract catches accidental
// removal of Kind or InsertOpts during future migrations.
var (
	_ river.JobArgsWithInsertOpts = IngestJobArgs{}
	_ river.JobArgsWithInsertOpts = EpgSyncArgs{}
	_ river.JobArgsWithInsertOpts = TunerSyncArgs{}
	_ river.JobArgsWithInsertOpts = RulerPassArgs{}
	_ river.JobArgsWithInsertOpts = ReconcilePassArgs{}
	_ river.JobArgsWithInsertOpts = RecordSweepArgs{}
	_ river.JobArgsWithInsertOpts = EncodeJobArgs{}
	_ river.JobArgsWithInsertOpts = EncodeEnqueueHintArgs{}
	_ river.JobArgsWithInsertOpts = ThumbnailJobArgs{}
	_ river.JobArgsWithInsertOpts = EncodeReconcileArgs{}
	_ river.JobArgsWithInsertOpts = DeleteReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CatalogExportArgs{}
	_ river.JobArgsWithInsertOpts = StorageSyncArgs{}
)
