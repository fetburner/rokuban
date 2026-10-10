package jobs

import (
	"time"

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
//
// **Queue は site で修飾しない**（ingest/epg/reconciler/watcher と異なる）。
// ruler は mirakc に一切触れない DB のみの仕事で、site 単位の到達性ガードが
// 要らない（issue #185 M4-13、issue #138 の決定表）。
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

// EncodeJobArgs は encode ジョブの引数。recording、プロファイル、rescue 用の締切を指定する。
type EncodeJobArgs struct {
	RecordingID int64         `json:"recording_id" river:"unique"`
	Profile     string        `json:"profile" river:"unique"`
	Timeout     time.Duration `json:"timeout,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeJobArgs) Kind() string { return "encode" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
func (EncodeJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: EncodeQueue,
		// 停止による Canceled は River の attempt を消費するがドメインでは数えない。
		// 26 > 25 は River が先に discard しない保証ではない（reconcile が新ジョブで回復する）。
		MaxAttempts: 26,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// EncodeEnqueueHintArgs は事後追加されたエンコードプロファイルを反映する
// ヒントジョブの引数。api がヒント経由にしているのは、実行（不足分の encode
// ジョブ投入）を常に worker ロールの中で完結させ、api が worker の実行ロジックを
// 直接呼ぶ経路を増やさないため（RulerPassArgs と同じ結合パターン）。
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

// SeekTilesJobArgs はシークプレビュー用タイル画像（kind = 'seek_tiles'）ジョブの
// 引数。
type SeekTilesJobArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind は River ジョブの種別名を返す。
func (SeekTilesJobArgs) Kind() string { return "seek_tiles" }

// InsertOpts は thumbnail キューへ投入するための River 挿入オプションを返す。
//
// **poster（thumbnail）と同じキューに載せるが、ジョブ種は分ける。** タイル側の
// 失敗で poster まで作り直させたくない（docs/storage/contract.md §5.1）。CPU を
// 食う仕事である点は同じなので、キューを分けて並列度の勘定を 2 つに割る理由は無い。
//
// **priority を poster より下げる。** River は priority → scheduled_at の順に
// 取り出し、thumbnail キューの既定の同時実行数は 1 である。同じ priority だと、
// 導入直後に定期パスが積む既存録画ぶん（最大 RowLimit 件）のタイルが片付くまで、
// 後から ingest された録画の poster が一覧に出ない。下げても、いま走っている
// 1 件ぶんの待ちは残る。
func (SeekTilesJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:    ThumbnailQueue,
		Priority: 4,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// ThumbnailReconcileArgs は thumbnail の desired−observed 定期 reconcile ジョブの
// 引数。thumbnail キューは実ジョブと共有するが、River の pending 一意性で定期
// パス同士が重ならないようにする。seek_tiles のギャップもこのパスが埋める
// （poster と違って一覧の表示を待たせる仕事ではないので、投入口を分けない）。
type ThumbnailReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (ThumbnailReconcileArgs) Kind() string { return "thumbnail_reconcile" }

// InsertOpts は thumbnail キューへ投入するための River 挿入オプションを返す。
func (ThumbnailReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: ThumbnailQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// CMDetectJobArgs identifies a recording whose CM ranges should be detected.
type CMDetectJobArgs struct {
	RecordingID         int64 `json:"recording_id" river:"unique"`
	RecordingDurationMs int64 `json:"recording_duration_ms"`
}

// Kind returns the River job kind.
func (CMDetectJobArgs) Kind() string { return "cm_detect" }

// InsertOpts routes CM detection to its dedicated queue with a short retry budget.
func (CMDetectJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       CMDetectQueue,
		MaxAttempts: 10,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: pendingJobStates},
	}
}

// CMDetectReconcileArgs requests one desired-minus-observed CM detection pass.
type CMDetectReconcileArgs struct{}

// Kind returns the River job kind.
func (CMDetectReconcileArgs) Kind() string { return "cm_detect_reconcile" }

// InsertOpts routes reconciliation to the CM detection queue.
func (CMDetectReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:      CMDetectQueue,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: pendingJobStates},
	}
}

// CMLogoCandidateJobArgs identifies one station-area analysis. The area version
// prevents an older hint from analyzing a recording after the user has saved a
// newer area.
type CMLogoCandidateJobArgs struct {
	NetworkID           int32     `json:"network_id" river:"unique"`
	ServiceID           int32     `json:"service_id" river:"unique"`
	RecordingID         int64     `json:"recording_id" river:"unique"`
	AreaUpdatedAt       time.Time `json:"area_updated_at" river:"unique"`
	RecordingDurationMs int64     `json:"recording_duration_ms"`
}

// Kind returns the River job kind.
func (CMLogoCandidateJobArgs) Kind() string { return "cm_logo_candidate" }

// InsertOpts routes candidate analysis to the CM detection queue with no retry:
// the user can save the area again to request a fresh analysis.
func (CMLogoCandidateJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       CMDetectQueue,
		MaxAttempts: 1,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: pendingJobStates},
	}
}

// LabelRuleReconcileArgs は分類ルールの変更を録画全件へ再評価するジョブの引数。
type LabelRuleReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (LabelRuleReconcileArgs) Kind() string { return "label_rule_reconcile" }

// InsertOpts は分類ルール再評価を cleanup キュー（site 非依存の DB ジョブ用。
// delete_reconcile / catalog_export と同じ）へ投入する。ruler キューは
// MaxWorkers 1 でサイトごとの ruler パスが並ぶので、全件再評価で塞がない。
//
// キューは site 非依存（再評価は site の属性を持たない全件の仕事で、site 単位に
// 回すと同じ評価を N 回走らせることになる）。
//
// **一意化しない（ByArgs / ByState を使わない）。** 実行中に来た 2 本目の
// ルール編集を捨てないためで、捨てると 1 本目が古いルール集合で評価し終えた
// 時点で打ち止めになり、2 本目の編集が次の定期再評価（15 分）まで反映されない。
//
// 「実行中を除いた状態集合」で代用できないのは River がそれを拒否するからで、
// UniqueOpts.ByState は running を含まない集合を挿入時にエラーにする
// （river@v0.47.0 insert_opts.go の requiredV3states。この検査を外すと
// `rokuban enqueue label-rule-reconcile` とルール編集の両方が 500 になる）。
//
// 代償は、素早く N 回編集すると N 回の全件評価が直列に走ること（1 回 0.86 s /
// 73,000 行）。編集はまれな操作なので許容する。advisory lock が直列化するので、
// 最後に走る 1 本は必ず最新のルール集合で評価する。
func (LabelRuleReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CleanupQueue}
}

// EncodeReconcileArgs は encode の desired−observed 定期 reconcile ジョブの引数。
type EncodeReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (EncodeReconcileArgs) Kind() string { return "encode_reconcile" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
//
// River のキュー単位の MaxWorkers はジョブ種を区別しないため、
// `encode.concurrency: 1`（既定）の構成では実行中の encode ジョブが終わるまで
// このパスは走らない。許容する: エンコードが詰まっている系では今すぐ投入しても
// 実行されないので、検出が遅れても失うものが無い。
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
//
// Keep が 0 以下なら catalog.DefaultKeep（7）を使う。
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
//
// キューは専用の StorageQueue にする。CleanupQueue は「物理削除系ジョブ専用」と
// 明記されているため、削除を一切しない観測ジョブをそこに混ぜない。
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

// 全契約型が River のジョブ引数インタフェースを満たすことのコンパイル時検査。
//
// 満たすべきは JobArgs（Kind だけ）ではなく JobArgsWithInsertOpts である。
// JobArgs で書くと InsertOpts を消してもコンパイルが通り、そのジョブは黙って
// default キューへ行く。deploy/k8s の ScaledJob は default を数えるトリガを
// 持たないので、ロール分割デプロイでは誰にも実行されないまま滞留する。
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
	_ river.JobArgsWithInsertOpts = SeekTilesJobArgs{}
	_ river.JobArgsWithInsertOpts = ThumbnailReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CMDetectJobArgs{}
	_ river.JobArgsWithInsertOpts = CMDetectReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CMLogoCandidateJobArgs{}
	_ river.JobArgsWithInsertOpts = EncodeReconcileArgs{}
	_ river.JobArgsWithInsertOpts = LabelRuleReconcileArgs{}
	_ river.JobArgsWithInsertOpts = DeleteReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CatalogExportArgs{}
	_ river.JobArgsWithInsertOpts = StorageSyncArgs{}
)
