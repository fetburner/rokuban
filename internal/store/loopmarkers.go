package store

import (
	"context"
	"time"
)

// loop の名前。ReadLoopMarkers の loop に入れる値で、loop_success_markers の loop 列と一致する。
// scope の意味は loop ごとに決まる（LoopRuler / LoopRecordSweep は site、LoopDeleteReconcile は置き場、
// LoopReconcilePass は下の全件パスの名前）。
const (
	LoopRuler           = "ruler"
	LoopRecordSweep     = "record_sweep"
	LoopReconcilePass   = "reconcile_pass"
	LoopDeleteReconcile = "delete_reconcile"
)

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
	// ReadLoopMarkers は loop の scope ごとの成功時刻を返す。一度も成功していない scope は含まない。
	ReadLoopMarkers(ctx context.Context, loop string) (map[string]time.Time, error)
}
