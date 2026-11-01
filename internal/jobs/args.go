package jobs

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

// IngestJobArgs は ingest ジョブの引数。mirakc サイトと record ID を指定する。
type IngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

// QueueSite は site 修飾に使う site を返す。
func (a IngestJobArgs) QueueSite() string { return a.Site }

// Kind はジョブ種別名を返す。
func (IngestJobArgs) Kind() string { return "ingest" }

// EpgSyncArgs は EPG 全量同期ジョブの引数。
type EpgSyncArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a EpgSyncArgs) QueueSite() string { return a.Site }

// Kind はジョブ種別名を返す。
func (EpgSyncArgs) Kind() string { return "epg_sync" }

// TunerSyncArgs はチューナー射影ジョブの引数。
type TunerSyncArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a TunerSyncArgs) QueueSite() string { return a.Site }

// Kind はジョブ種別名を返す。
func (TunerSyncArgs) Kind() string { return "tuner_sync" }

// RulerPassArgs は ruler の 1 パスジョブの引数。
type RulerPassArgs struct {
	Site string `json:"site"`
}

// Kind はジョブ種別名を返す。
func (RulerPassArgs) Kind() string { return "ruler_pass" }

// ReconcilePassArgs は reconciler の 1 パス突き合わせジョブの引数。
type ReconcilePassArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a ReconcilePassArgs) QueueSite() string { return a.Site }

// Kind はジョブ種別名を返す。
func (ReconcilePassArgs) Kind() string { return "reconcile_pass" }

// RecordSweepArgs は watcher の定期全量突き合わせジョブの引数。
type RecordSweepArgs struct {
	Site string `json:"site"`
}

// QueueSite は site 修飾に使う site を返す。
func (a RecordSweepArgs) QueueSite() string { return a.Site }

// Kind はジョブ種別名を返す。
func (RecordSweepArgs) Kind() string { return "record_sweep" }

// EncodeJobArgs は encode ジョブの引数。recording、プロファイル、rescue 用の締切を指定する。
type EncodeJobArgs struct {
	RecordingID      int64           `json:"recording_id"`
	Profile          string          `json:"profile"`
	Timeout          time.Duration   `json:"timeout,omitempty"`
	RequestedCutKeep json.RawMessage `json:"requested_cut_keep,omitempty"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a EncodeJobArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (EncodeJobArgs) Kind() string { return "encode" }

// ThumbnailJobArgs は thumbnail ジョブの引数。
type ThumbnailJobArgs struct {
	RecordingID int64 `json:"recording_id"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a ThumbnailJobArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (ThumbnailJobArgs) Kind() string { return "thumbnail" }

// SeekTilesJobArgs はシークプレビュー用タイル画像（kind = 'seek_tiles'）ジョブの
// 引数。
type SeekTilesJobArgs struct {
	RecordingID int64 `json:"recording_id"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a SeekTilesJobArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (SeekTilesJobArgs) Kind() string { return "seek_tiles" }

// ThumbnailReconcileArgs は thumbnail の desired−observed reconcile ジョブの引数。
// RecordingID が 0 なら全件パス、非 0 なら指定録画だけを確認する。
// After* は全件パスの継続位置で、*Done は継続チェーンで短いページを返した窓を表す。
// 対象録画の指定と継続状態を同時に載せる経路は無い。
type ThumbnailReconcileArgs struct {
	RecordingID               int64 `json:"recording_id,omitempty"`
	AfterRecordingID          int64 `json:"after_recording_id,omitempty"`
	AfterReselectRecordingID  int64 `json:"after_reselect_recording_id,omitempty"`
	AfterSeekTilesRecordingID int64 `json:"after_seek_tiles_recording_id,omitempty"`
	MissingDone               bool  `json:"missing_done,omitempty"`
	ReselectDone              bool  `json:"reselect_done,omitempty"`
	SeekTilesDone             bool  `json:"seek_tiles_done,omitempty"`
}

// Kind はジョブ種別名を返す。
func (ThumbnailReconcileArgs) Kind() string { return "thumbnail_reconcile" }

// CMDetectJobArgs identifies a recording whose CM ranges should be detected.
type CMDetectJobArgs struct {
	RecordingID         int64 `json:"recording_id"`
	RecordingDurationMs int64 `json:"recording_duration_ms"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a CMDetectJobArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (CMDetectJobArgs) Kind() string { return "cm_detect" }

// CMDetectReconcileArgs requests one desired-minus-observed CM detection pass.
type CMDetectReconcileArgs struct{}

// Kind はジョブ種別名を返す。
func (CMDetectReconcileArgs) Kind() string { return "cm_detect_reconcile" }

// CMLogoCandidateJobArgs identifies one station-area analysis. The area version
// prevents an older hint from analyzing a recording after the user has saved a
// newer area.
type CMLogoCandidateJobArgs struct {
	NetworkID           int32     `json:"network_id"`
	ServiceID           int32     `json:"service_id"`
	RecordingID         int64     `json:"recording_id"`
	AreaUpdatedAt       time.Time `json:"area_updated_at"`
	RecordingDurationMs int64     `json:"recording_duration_ms"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a CMLogoCandidateJobArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (CMLogoCandidateJobArgs) Kind() string { return "cm_logo_candidate" }

// LabelRuleReconcileArgs は分類ルールの変更を録画全件へ再評価するジョブの引数。
type LabelRuleReconcileArgs struct{}

// Kind はジョブ種別名を返す。
func (LabelRuleReconcileArgs) Kind() string { return "label_rule_reconcile" }

// EncodeReconcileArgs は encode の desired−observed reconcile ジョブの引数。
// RecordingID が 0 なら全件パス、非 0 なら指定録画だけを確認する。
// AfterRecordingID は全件パスの継続位置で、RecordingID と同時には指定しない。
type EncodeReconcileArgs struct {
	RecordingID      int64 `json:"recording_id,omitempty"`
	AfterRecordingID int64 `json:"after_recording_id,omitempty"`
}

// Kind はジョブ種別名を返す。
func (EncodeReconcileArgs) Kind() string { return "encode_reconcile" }

// DeleteReconcileArgs は削除 reconcile ジョブの引数。
type DeleteReconcileArgs struct {
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a DeleteReconcileArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (DeleteReconcileArgs) Kind() string { return "delete_reconcile" }

// CatalogExportArgs は catalog エクスポートジョブの引数。
//
// Keep が 0 以下なら catalog.DefaultKeep（7）を使う。
type CatalogExportArgs struct {
	Keep int `json:"keep,omitempty"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a CatalogExportArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (CatalogExportArgs) Kind() string { return "catalog_export" }

// StorageSyncArgs はストレージ観測ジョブの引数。
//
// キューは専用の StorageQueue にする。CleanupQueue は「物理削除系ジョブ専用」と
// 明記されているため、削除を一切しない観測ジョブをそこに混ぜない。
type StorageSyncArgs struct {
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `{queue}_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore は実行する置き場のキューの置き場を返す。
func (a StorageSyncArgs) QueueStore() string { return a.Store }

// Kind はジョブ種別名を返す。
func (StorageSyncArgs) Kind() string { return "storage_sync" }

// StorageTransferReconcileArgs requests a level-triggered copy placement pass.
type StorageTransferReconcileArgs struct{}

// Kind はジョブ種別名を返す。
func (StorageTransferReconcileArgs) Kind() string { return "storage_transfer_reconcile" }

// StorageTransferArgs は desired store へコピーする論理資産を指定する。
//
// 宛先が FS の置き場なら SourceStore は空で、キューは宛先の置き場で修飾される。宛先が S3 の派生物の置き場なら、
// 転送は転送元の FS 置き場の worker が S3 へ直接送るので、SourceStore にその置き場を入れてキューを選ぶ。
type StorageTransferArgs struct {
	MediaAssetID int64  `json:"media_asset_id"`
	Store        string `json:"store"`
	// SourceStore は S3 宛ての転送を実行する FS 置き場。省略時（旧いジョブを含む）は Store のキューに載る。
	SourceStore       string `json:"source_store,omitempty"`
	PolicyFingerprint string `json:"policy_fingerprint"`
}

// QueueStore は、このジョブを載せる物理キューの置き場を返す。SourceStore があればそれ、無ければ書き込み先の Store。
func (a StorageTransferArgs) QueueStore() string {
	if a.SourceStore != "" {
		return a.SourceStore
	}
	return a.Store
}

// Kind はジョブ種別名を返す。
func (StorageTransferArgs) Kind() string { return "storage_transfer" }

// siteBound は site 束縛キューに載る Args が実装する。site 修飾に使う site の取り出しは
// この 1 か所だけにする。RulerPassArgs は site 単位キューに載らないため実装しない。
type siteBound interface{ QueueSite() string }

type storeBound interface{ QueueStore() string }

// PhysicalQueueFor は args が実際に投入される物理キュー名を返す。
// site 束縛キューの Args が QueueSite を実装しない場合は error を返す。
func PhysicalQueueFor(args jobq.Args) (string, error) {
	if args == nil {
		return "", fmt.Errorf("jobs: args are nil")
	}
	logical := args.Spec().Queue
	if IsStoreBound(logical) {
		bound, ok := args.(storeBound)
		if !ok {
			return "", fmt.Errorf("jobs: %s queue args %T do not implement QueueStore", logical, args)
		}
		storeName := bound.QueueStore()
		if storeName == "" {
			// River validates a registered worker with its zero-value Args. Route that
			// registration probe to the same fallback name used for an unbound site.
			storeName = store.DefaultSite
		}
		if !mediapath.ValidStoreName(storeName) {
			return "", fmt.Errorf("jobs: %s queue args %T contain invalid store name %q", logical, args, storeName)
		}
		physical := PhysicalStoreQueueName(logical, storeName)
		if len(physical) > jobq.MaxQueueNameLen {
			return "", fmt.Errorf("jobs: physical queue name %q exceeds %d characters", physical, jobq.MaxQueueNameLen)
		}
		return physical, nil
	}
	if slices.Contains(siteBoundQueueNames, logical) {
		bound, ok := args.(siteBound)
		if !ok {
			return "", fmt.Errorf("jobs: %s queue args %T do not implement QueueSite", logical, args)
		}
		return PhysicalQueueName(logical, bound.QueueSite()), nil
	}
	return logical, nil
}

// 全 Args が jobq.Args を満たすことのコンパイル時検査。
var (
	_ jobq.Args = IngestJobArgs{}
	_ jobq.Args = EpgSyncArgs{}
	_ jobq.Args = TunerSyncArgs{}
	_ jobq.Args = RulerPassArgs{}
	_ jobq.Args = ReconcilePassArgs{}
	_ jobq.Args = RecordSweepArgs{}
	_ jobq.Args = EncodeJobArgs{}
	_ jobq.Args = ThumbnailJobArgs{}
	_ jobq.Args = SeekTilesJobArgs{}
	_ jobq.Args = ThumbnailReconcileArgs{}
	_ jobq.Args = CMDetectJobArgs{}
	_ jobq.Args = CMDetectReconcileArgs{}
	_ jobq.Args = CMLogoCandidateJobArgs{}
	_ jobq.Args = EncodeReconcileArgs{}
	_ jobq.Args = LabelRuleReconcileArgs{}
	_ jobq.Args = DeleteReconcileArgs{}
	_ jobq.Args = CatalogExportArgs{}
	_ jobq.Args = StorageSyncArgs{}
	_ jobq.Args = StorageTransferReconcileArgs{}
	_ jobq.Args = StorageTransferArgs{}
)
