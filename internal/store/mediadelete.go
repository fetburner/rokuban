package store

import (
	"context"
	"time"
)

// MediaDeleteAsset は物理削除の対象になった物理コピーを表す。
type MediaDeleteAsset struct {
	// ID は物理コピー行の ID。
	ID           int64
	MediaAssetID int64
	RecordingID  int64
	RelPath      string
	SizeBytes    int64
	Kind         string
	Generation   int64
}

// MediaDeleteRevertCandidate は削除条件から外れた deleting アセットを表す。
type MediaDeleteRevertCandidate struct {
	// ID は物理コピー行の ID。
	ID           int64
	MediaAssetID int64
	RecordingID  int64
	RelPath      string
	Kind         string
}

// MediaDelete は削除 reconcile がメディアアセットの削除状態を読み書きする契約である。
type MediaDelete interface {
	// ListPendingDeleteAssets は、削除条件を満たす deleting アセットを返す。
	// originalRules と derivedStore は、desired の置き場から外れた配置の腕が使う（転送が無ければ空の規則でよい）。
	ListPendingDeleteAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string, limit int32) ([]MediaDeleteAsset, error)
	// ListUnqualifiedDeletingAssets は、現在どの削除条件も満たさない deleting アセットを返す。
	ListUnqualifiedDeletingAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string, limit int32) ([]MediaDeleteRevertCandidate, error)
	// ListOldGenerationMediaAssetCopiesToDelete は overlap を過ぎた旧世代コピーを返す。
	ListOldGenerationMediaAssetCopiesToDelete(ctx context.Context, overlapCutoff time.Time, limit int32) ([]MediaDeleteAsset, error)
	// ListTrashAssetsToDelete は、ごみ箱の猶予を過ぎたか即時削除を要求されたアセットを返す。
	ListTrashAssetsToDelete(ctx context.Context, graceCutoff time.Time, limit int32) ([]MediaDeleteAsset, error)
	// ListUntilEncodedOriginalsToDelete は、派生物が揃った until_encoded 原本を返す。
	ListUntilEncodedOriginalsToDelete(ctx context.Context, limit int32) ([]MediaDeleteAsset, error)
	// ListRemovedEncodedAssetsToDelete は、利用者が外した encoded アセットを返す。
	ListRemovedEncodedAssetsToDelete(ctx context.Context, limit int32) ([]MediaDeleteAsset, error)
	// MarkDeleting は active アセットを deleting にする。
	MarkDeleting(ctx context.Context, copyID int64, expectedState string) (bool, error)
	// MarkUntilEncodedDeleting は削除条件を再確認して原本を deleting にする。
	// recordingID の録画に対する削除方針・作り直し要求の書き込みと直列化する。
	MarkUntilEncodedDeleting(ctx context.Context, recordingID, mediaAssetID, copyID int64) (bool, error)
	// MarkDeleted は deleting アセットを deleted にし、外した encoded の削除要求も同時に消す。
	MarkDeleted(ctx context.Context, copyID int64) error
	// RevertToActive は削除条件を再確認し、該当しなくなった deleting アセットを active に戻す。
	RevertToActive(ctx context.Context, copyID int64, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string) (bool, error)
}

// PendingCopyReclaim は、公開されないまま猶予を過ぎた pending のコピー行の回収を担う契約である。
// pending は一度も現行のコピーになっていないので、deleting を経由せず、行をロックしたまま key を消す。
type PendingCopyReclaim interface {
	// ListAgedPendingMediaAssetCopies は、before より前に予約された pending の派生物コピーを返す。
	ListAgedPendingMediaAssetCopies(ctx context.Context, before time.Time, limit int32) ([]MediaDeleteAsset, error)
	// ReclaimPendingMediaAssetCopy は pending の行をロックし、まだ before より前の pending なら
	// deleteObject を呼び、成功したら deleted にする。公開が先に active にしていれば何もせず false を返す。
	// 公開の tx は同じ行を更新するので、deleteObject が走る間は active にならない。
	ReclaimPendingMediaAssetCopy(ctx context.Context, copyID int64, before time.Time, deleteObject func() error) (bool, error)
}
