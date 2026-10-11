package jobs

import (
	"fmt"
	"slices"
	"time"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobq"
)

// IngestJobArgs は ingest ジョブの引数。mirakc サイトと record ID を指定する。
type IngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

// QueueSite は site 修飾に使う site を返す。
func (a IngestJobArgs) QueueSite() string { return a.Site }

// Kind は River ジョブの種別名を返す。
func (IngestJobArgs) Kind() string { return "ingest" }

// InsertOpts は ingest キューへ投入するための River 挿入オプションを返す。
func (a IngestJobArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(a)
}

// EpgSyncArgs は EPG 全量同期ジョブの引数。
type EpgSyncArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a EpgSyncArgs) QueueSite() string { return a.Site }

// Kind は River ジョブの種別名を返す。
func (EpgSyncArgs) Kind() string { return "epg_sync" }

// InsertOpts は EPG キューへ投入するための River 挿入オプションを返す。
func (a EpgSyncArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(a)
}

// TunerSyncArgs はチューナー射影ジョブの引数。
type TunerSyncArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a TunerSyncArgs) QueueSite() string { return a.Site }

// Kind は River ジョブの種別名を返す。
func (TunerSyncArgs) Kind() string { return "tuner_sync" }

// InsertOpts は EPG キューへ投入するための River 挿入オプションを返す。
func (a TunerSyncArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(a)
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
	return riverInsertOpts(RulerPassArgs{})
}

// ReconcilePassArgs は reconciler の 1 パス突き合わせジョブの引数。
type ReconcilePassArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a ReconcilePassArgs) QueueSite() string { return a.Site }

// Kind は River ジョブの種別名を返す。
func (ReconcilePassArgs) Kind() string { return "reconcile_pass" }

// InsertOpts は site 修飾済みの reconciler キューへ投入するための River 挿入
// オプションを返す。
func (a ReconcilePassArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(a)
}

// RecordSweepArgs は watcher の定期全量突き合わせジョブの引数。
type RecordSweepArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a RecordSweepArgs) QueueSite() string { return a.Site }

// Kind は River ジョブの種別名を返す。
func (RecordSweepArgs) Kind() string { return "record_sweep" }

// InsertOpts は site 修飾済みの watcher キューへ投入するための River 挿入
// オプションを返す。
func (a RecordSweepArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(a)
}

// EncodeJobArgs は encode ジョブの引数。recording、プロファイル、rescue 用の締切を指定する。
type EncodeJobArgs struct {
	RecordingID int64         `json:"recording_id"`
	Profile     string        `json:"profile"`
	Timeout     time.Duration `json:"timeout,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeJobArgs) Kind() string { return "encode" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
func (EncodeJobArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(EncodeJobArgs{})
}

// ThumbnailJobArgs は thumbnail ジョブの引数。
type ThumbnailJobArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind は River ジョブの種別名を返す。
func (ThumbnailJobArgs) Kind() string { return "thumbnail" }

// InsertOpts は thumbnail キューへ投入するための River 挿入オプションを返す。
func (ThumbnailJobArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(ThumbnailJobArgs{})
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
	return riverInsertOpts(SeekTilesJobArgs{})
}

// ThumbnailReconcileArgs は thumbnail の desired−observed reconcile ジョブの引数。
// RecordingID が 0 なら全件パス、非 0 なら指定録画だけを確認する。
type ThumbnailReconcileArgs struct {
	RecordingID int64 `json:"recording_id,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (ThumbnailReconcileArgs) Kind() string { return "thumbnail_reconcile" }

// InsertOpts は thumbnail キューへ投入するための River 挿入オプションを返す。
func (ThumbnailReconcileArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(ThumbnailReconcileArgs{})
}

// CMDetectJobArgs identifies a recording whose CM ranges should be detected.
type CMDetectJobArgs struct {
	RecordingID         int64 `json:"recording_id"`
	RecordingDurationMs int64 `json:"recording_duration_ms"`
}

// Kind returns the River job kind.
func (CMDetectJobArgs) Kind() string { return "cm_detect" }

// InsertOpts routes CM detection to its dedicated queue with a short retry budget.
func (CMDetectJobArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(CMDetectJobArgs{})
}

// CMDetectReconcileArgs requests one desired-minus-observed CM detection pass.
type CMDetectReconcileArgs struct{}

// Kind returns the River job kind.
func (CMDetectReconcileArgs) Kind() string { return "cm_detect_reconcile" }

// InsertOpts routes reconciliation to the CM detection queue.
func (CMDetectReconcileArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(CMDetectReconcileArgs{})
}

// CMLogoCandidateJobArgs identifies one station-area analysis. The area version
// prevents an older hint from analyzing a recording after the user has saved a
// newer area.
type CMLogoCandidateJobArgs struct {
	NetworkID           int32     `json:"network_id"`
	ServiceID           int32     `json:"service_id"`
	RecordingID         int64     `json:"recording_id"`
	AreaUpdatedAt       time.Time `json:"area_updated_at"`
	RecordingDurationMs int64     `json:"recording_duration_ms"`
}

// Kind returns the River job kind.
func (CMLogoCandidateJobArgs) Kind() string { return "cm_logo_candidate" }

// InsertOpts routes candidate analysis to the CM detection queue with no retry:
// the user can save the area again to request a fresh analysis.
func (CMLogoCandidateJobArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(CMLogoCandidateJobArgs{})
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
	return riverInsertOpts(LabelRuleReconcileArgs{})
}

// EncodeReconcileArgs は encode の desired−observed reconcile ジョブの引数。
// RecordingID が 0 なら全件パス、非 0 なら指定録画だけを確認する。
type EncodeReconcileArgs struct {
	RecordingID int64 `json:"recording_id,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeReconcileArgs) Kind() string { return "encode_reconcile" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
//
// River のキュー単位の MaxWorkers はジョブ種を区別しないため、
// `encode.concurrency: 1`（既定）の構成では実行中の encode ジョブが終わるまで
// このパスは走らない。許容する: エンコードが詰まっている系では今すぐ投入しても
// 実行されないので、検出が遅れても失うものが無い。
func (EncodeReconcileArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(EncodeReconcileArgs{})
}

// EncodeRebuildArgs は利用者が明示した再エンコード命令の引数。
// reconcile は不足分だけを導出するため、既存の cut 版を置き換える命令は別種にする。
type EncodeRebuildArgs struct {
	RecordingID int64  `json:"recording_id"`
	Profile     string `json:"profile"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeRebuildArgs) Kind() string { return "encode_rebuild" }

// InsertOpts は encode キューへ投入するための River 挿入オプションを返す。
// 同じ録画・プロファイルへの待機中または実行中の要求を合流させる。
func (EncodeRebuildArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(EncodeRebuildArgs{})
}

// DeleteReconcileArgs は削除 reconcile ジョブの引数。
type DeleteReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (DeleteReconcileArgs) Kind() string { return "delete_reconcile" }

// InsertOpts は cleanup キューへ投入するための River 挿入オプションを返す。
func (DeleteReconcileArgs) InsertOpts() river.InsertOpts {
	return riverInsertOpts(DeleteReconcileArgs{})
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
	return riverInsertOpts(CatalogExportArgs{})
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
	return riverInsertOpts(StorageSyncArgs{})
}

func riverInsertOpts(args jobq.Args) river.InsertOpts {
	queue, err := PhysicalQueueFor(args)
	if err != nil {
		panic(err)
	}
	return RiverInsertOpts(args.Spec(), queue)
}

// siteBound は site 束縛キューに載る Args が実装する。site 修飾に使う site の取り出しは
// この 1 か所だけにする。RulerPassArgs は意図的に実装しない（InsertOpts の注記参照）。
type siteBound interface{ QueueSite() string }

// PhysicalQueueFor は args が実際に投入される物理キュー名を返す。
// site 束縛キューの Args が QueueSite を実装しない場合は error を返す。
func PhysicalQueueFor(args jobq.Args) (string, error) {
	if args == nil {
		return "", fmt.Errorf("jobs: args are nil")
	}
	logical := args.Spec().Queue
	if slices.Contains(siteBoundQueueNames, logical) {
		bound, ok := args.(siteBound)
		if !ok {
			return "", fmt.Errorf("jobs: %s queue args %T do not implement QueueSite", logical, args)
		}
		return PhysicalQueueName(logical, bound.QueueSite()), nil
	}
	return logical, nil
}

// 全 Args が jobq 契約と旧 River ジョブ引数インタフェースを満たすことのコンパイル時検査。
//
// 満たすべきは JobArgs（Kind だけ）ではなく JobArgsWithInsertOpts である。
// JobArgs で書くと InsertOpts を消してもコンパイルが通り、そのジョブは黙って
// default キューへ行く。deploy/k8s の ScaledJob は default を数えるトリガを
// 持たないので、ロール分割デプロイでは誰にも実行されないまま滞留する。
var (
	_ jobq.Args                   = IngestJobArgs{}
	_ jobq.Args                   = EpgSyncArgs{}
	_ jobq.Args                   = TunerSyncArgs{}
	_ jobq.Args                   = RulerPassArgs{}
	_ jobq.Args                   = ReconcilePassArgs{}
	_ jobq.Args                   = RecordSweepArgs{}
	_ jobq.Args                   = EncodeJobArgs{}
	_ jobq.Args                   = ThumbnailJobArgs{}
	_ jobq.Args                   = SeekTilesJobArgs{}
	_ jobq.Args                   = ThumbnailReconcileArgs{}
	_ jobq.Args                   = CMDetectJobArgs{}
	_ jobq.Args                   = CMDetectReconcileArgs{}
	_ jobq.Args                   = CMLogoCandidateJobArgs{}
	_ jobq.Args                   = EncodeReconcileArgs{}
	_ jobq.Args                   = EncodeRebuildArgs{}
	_ jobq.Args                   = LabelRuleReconcileArgs{}
	_ jobq.Args                   = DeleteReconcileArgs{}
	_ jobq.Args                   = CatalogExportArgs{}
	_ jobq.Args                   = StorageSyncArgs{}
	_ river.JobArgsWithInsertOpts = IngestJobArgs{}
	_ river.JobArgsWithInsertOpts = EpgSyncArgs{}
	_ river.JobArgsWithInsertOpts = TunerSyncArgs{}
	_ river.JobArgsWithInsertOpts = RulerPassArgs{}
	_ river.JobArgsWithInsertOpts = ReconcilePassArgs{}
	_ river.JobArgsWithInsertOpts = RecordSweepArgs{}
	_ river.JobArgsWithInsertOpts = EncodeJobArgs{}
	_ river.JobArgsWithInsertOpts = ThumbnailJobArgs{}
	_ river.JobArgsWithInsertOpts = SeekTilesJobArgs{}
	_ river.JobArgsWithInsertOpts = ThumbnailReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CMDetectJobArgs{}
	_ river.JobArgsWithInsertOpts = CMDetectReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CMLogoCandidateJobArgs{}
	_ river.JobArgsWithInsertOpts = EncodeReconcileArgs{}
	_ river.JobArgsWithInsertOpts = EncodeRebuildArgs{}
	_ river.JobArgsWithInsertOpts = LabelRuleReconcileArgs{}
	_ river.JobArgsWithInsertOpts = DeleteReconcileArgs{}
	_ river.JobArgsWithInsertOpts = CatalogExportArgs{}
	_ river.JobArgsWithInsertOpts = StorageSyncArgs{}
)
