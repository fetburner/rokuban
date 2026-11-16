package store

import (
	"context"
	"time"
)

// 全件パスの名前。ReconcilePassMarkers の pass に入れる値で、置き場を持たないパスだけを並べる。
const (
	ReconcilePassEncode    = "encode"
	ReconcilePassThumbnail = "thumbnail"
	ReconcilePassTSScan    = "ts_scan"
)

// ReconcilePassMarkers は desired−observed の全件パスが完走した時刻を記録し、監視向けに読み取る契約である。
// 全件パスとは targeted pass（recording_id 指定）以外の実行を指す。
type ReconcilePassMarkers interface {
	// MarkReconcilePass は pass の全件パスが完走した時刻を今の時刻で保存する。
	MarkReconcilePass(ctx context.Context, pass string) error
	// ReadReconcilePassMarkers は一度でも完走した pass の時刻を返す。完走していない pass は含まない。
	ReadReconcilePassMarkers(ctx context.Context) (map[string]time.Time, error)
}

// DeleteReconcileMarkers は置き場ごとの削除 reconcile が完走した時刻を記録し、監視向けに読み取る契約である。
type DeleteReconcileMarkers interface {
	// MarkDeleteReconcile は置き場 store の削除 reconcile が完走した時刻を今の時刻で保存する。
	MarkDeleteReconcile(ctx context.Context, store string) error
	// ReadDeleteReconcileMarkers は一度でも完走した置き場の時刻を返す。完走していない置き場は含まない。
	ReadDeleteReconcileMarkers(ctx context.Context) (map[string]time.Time, error)
}

// StorageSyncReader は置き場ごと・root ごとの最後の観測時刻を読む契約である。
// 書き込みは StorageObservation が持つ。監視の読み取りだけを切り出す。
type StorageSyncReader interface {
	// ListStorageSync は全置き場の root 別観測値を返す。
	ListStorageSync(ctx context.Context) ([]StorageSync, error)
}
