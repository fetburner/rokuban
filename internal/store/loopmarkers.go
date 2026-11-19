package store

import (
	"context"
	"fmt"
	"time"
)

// loop の名前。ReadLoopMarkers の loop に入れる値で、loop_success_markers の loop 列と一致する。
// scope の意味は loop ごとに決まる（LoopRuler / LoopRecordSweep / LoopReconciler / LoopEPGSync /
// LoopTunerSync は site、LoopDeleteReconcile は置き場、LoopReconcilePass は下の全件パスの名前、
// LoopStorageTransfer は StorageTransferScope）。
// LoopReconciler は予約同期（reconciler ジョブ）で、全件パスの LoopReconcilePass とは別の loop。
// LoopEPGSync / LoopTunerSync / LoopStorageTransfer は ScaledJob の書き手の印で、常駐プロセスの
// 監視が読む（scope の決め方は loop_success_markers の migration の冒頭を参照）。
const (
	LoopRuler           = "ruler"
	LoopRecordSweep     = "record_sweep"
	LoopReconcilePass   = "reconcile_pass"
	LoopDeleteReconcile = "delete_reconcile"
	LoopReconciler      = "reconciler"
	LoopEPGSync         = "epg_sync"
	LoopTunerSync       = "tuner_sync"
	LoopStorageTransfer = "storage_transfer"
)

// StorageTransferScope は LoopStorageTransfer の唯一の scope。物理コピーの成功は置き場を問わず
// 1 行に置く（系列はラベルを持たない）。書き込みは StorageTransfer.CompleteMediaAssetTransfer の
// トランザクションの中で行い、この契約の MarkXxx には含めない。
const StorageTransferScope = "all"

// 全件パスの名前。MarkReconcilePass の pass に入れる値で、置き場を持たないパスだけを並べる。
const (
	ReconcilePassEncode    = "encode"
	ReconcilePassThumbnail = "thumbnail"
	ReconcilePassTSScan    = "ts_scan"
)

// LoopMarkers はループの成功時刻を記録し、監視向けに読み取る契約である。
// 全件パスとは targeted pass（recording_id 指定）以外の実行を指す。
type LoopMarkers interface {
	// MarkRulerPasses は成功した複数 site の ruler の時刻を 1 つのトランザクションで更新する。
	MarkRulerPasses(ctx context.Context, sites []string) error
	// MarkRecordSweep は site の record_sweep 成功時刻を更新する。
	MarkRecordSweep(ctx context.Context, site string) error
	// MarkReconcilePass は pass の全件パスが完走した時刻を今の時刻で保存する。
	MarkReconcilePass(ctx context.Context, pass string) error
	// MarkDeleteReconcile は置き場 store の削除 reconcile が完走した時刻を今の時刻で保存する。
	MarkDeleteReconcile(ctx context.Context, store string) error
	// MarkReconciler は site の予約同期（reconciler）のパスが完走した時刻を今の時刻で保存する。
	MarkReconciler(ctx context.Context, site string) error
	// MarkEPGSync は site の EPG 全量同期が確定した時刻を今の時刻で保存する。
	MarkEPGSync(ctx context.Context, site string) error
	// MarkTunerSync は site のチューナー全量同期が確定した時刻を今の時刻で保存する。
	MarkTunerSync(ctx context.Context, site string) error
	// ReadLoopMarkers は loop の scope ごとの成功時刻を返す。一度も成功していない scope は含まない。
	ReadLoopMarkers(ctx context.Context, loop string) (map[string]time.Time, error)
}

// MarkReconcilePass は全件パスの完走を記録する。
// worker の encode と thumbnail は完走の記録とエラーの扱いを共有する。
// 両パッケージが参照する LoopMarkers の契約と同じ store に置く。
// markers が nil なら何もしない（記録を呼ばない部分テストだけ）。
func MarkReconcilePass(ctx context.Context, markers LoopMarkers, pass string) error {
	if markers == nil {
		return nil
	}
	if err := markers.MarkReconcilePass(ctx, pass); err != nil {
		return fmt.Errorf("%s reconcile: marking completed pass: %w", pass, err)
	}
	return nil
}
