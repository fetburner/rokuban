package jobs

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
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
}

// Kind はジョブ種別名を返す。
func (EncodeJobArgs) Kind() string { return "encode" }

// ThumbnailJobArgs は thumbnail ジョブの引数。
type ThumbnailJobArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind はジョブ種別名を返す。
func (ThumbnailJobArgs) Kind() string { return "thumbnail" }

// SeekTilesJobArgs はシークプレビュー用タイル画像（kind = 'seek_tiles'）ジョブの
// 引数。
type SeekTilesJobArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind はジョブ種別名を返す。
func (SeekTilesJobArgs) Kind() string { return "seek_tiles" }

// ThumbnailReconcileArgs は thumbnail の desired−observed reconcile ジョブの引数。
// RecordingID が 0 なら全件パス、非 0 なら指定録画だけを確認する。
// After* は全件パスの継続位置で、対象録画の指定とは同時に使わない。
type ThumbnailReconcileArgs struct {
	RecordingID               int64 `json:"recording_id,omitempty"`
	AfterRecordingID          int64 `json:"after_recording_id,omitempty"`
	AfterReselectRecordingID  int64 `json:"after_reselect_recording_id,omitempty"`
	AfterSeekTilesRecordingID int64 `json:"after_seek_tiles_recording_id,omitempty"`
}

// Kind はジョブ種別名を返す。
func (ThumbnailReconcileArgs) Kind() string { return "thumbnail_reconcile" }

// CMDetectJobArgs identifies a recording whose CM ranges should be detected.
type CMDetectJobArgs struct {
	RecordingID         int64 `json:"recording_id"`
	RecordingDurationMs int64 `json:"recording_duration_ms"`
}

// Kind はジョブ種別名を返す。
func (CMDetectJobArgs) Kind() string { return "cm_detect" }

// CMDetectReconcileArgs requests one desired-minus-observed CM detection pass.
// The recording cursor and the composite station cursor advance independently.
type CMDetectReconcileArgs struct {
	AfterRecordingID        int64 `json:"after_recording_id,omitempty"`
	CandidateAfterNetworkID int32 `json:"candidate_after_network_id,omitempty"`
	CandidateAfterServiceID int32 `json:"candidate_after_service_id,omitempty"`
}

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
}

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
type DeleteReconcileArgs struct{}

// Kind はジョブ種別名を返す。
func (DeleteReconcileArgs) Kind() string { return "delete_reconcile" }

// CatalogExportArgs は catalog エクスポートジョブの引数。
//
// Keep が 0 以下なら catalog.DefaultKeep（7）を使う。
type CatalogExportArgs struct {
	Keep int `json:"keep,omitempty"`
}

// Kind はジョブ種別名を返す。
func (CatalogExportArgs) Kind() string { return "catalog_export" }

// StorageSyncArgs はストレージ観測ジョブの引数。
//
// キューは専用の StorageQueue にする。CleanupQueue は「物理削除系ジョブ専用」と
// 明記されているため、削除を一切しない観測ジョブをそこに混ぜない。
type StorageSyncArgs struct{}

// Kind はジョブ種別名を返す。
func (StorageSyncArgs) Kind() string { return "storage_sync" }

// siteBound は site 束縛キューに載る Args が実装する。site 修飾に使う site の取り出しは
// この 1 か所だけにする。RulerPassArgs は site 単位キューに載らないため実装しない。
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
)
