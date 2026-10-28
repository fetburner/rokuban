package store

import (
	"context"
	"time"
)

// MediaDeleteAsset は物理削除の対象になったメディアアセットを表す。
type MediaDeleteAsset struct {
	ID          int64
	RecordingID int64
	RelPath     string
	SizeBytes   int64
	Kind        string
}

// MediaDeleteRevertCandidate は削除条件から外れた deleting アセットを表す。
type MediaDeleteRevertCandidate struct {
	ID          int64
	CopyID      int64
	RecordingID int64
	RelPath     string
}

// MediaAssetCopy describes a physical copy awaiting cleanup.
type MediaAssetCopy struct {
	ID           int64
	MediaAssetID int64
	Store        string
	RelPath      string
	RecordingID  int64
	Kind         string
	SizeBytes    int64
}

// MediaCopyMaintenance recovers S3 copies from durable DB state.
type MediaCopyMaintenance interface {
	// ListAgedPendingMediaAssetCopies returns only pending copy rows older than before.
	ListAgedPendingMediaAssetCopies(ctx context.Context, before time.Time, limit int32) ([]MediaAssetCopy, error)
	// ListDeletingMediaAssetCopies returns keys whose copy rows already authorize deletion.
	ListDeletingMediaAssetCopies(ctx context.Context, limit int32) ([]MediaAssetCopy, error)
	// ClaimAgedPendingMediaAssetCopy atomically fences off a stale pending copy before object deletion.
	ClaimAgedPendingMediaAssetCopy(ctx context.Context, copyID int64, before time.Time) (bool, error)
	// MarkPendingMediaAssetCopyDeleted marks one pending-copy row reclaimed after its object is removed.
	MarkPendingMediaAssetCopyDeleted(ctx context.Context, copyID int64) error
	// MarkDeletingMediaAssetCopyDeleted marks one deleting-copy row removed and finalizes its logical asset if no copies remain.
	MarkDeletingMediaAssetCopyDeleted(ctx context.Context, copyID int64) error
}

// MediaDelete は削除 reconcile がメディアアセットの削除状態を読み書きする契約である。
type MediaDelete interface {
	// ListPendingDeleteAssets は、削除条件を満たす deleting アセットを返す。
	ListPendingDeleteAssets(ctx context.Context, graceCutoff time.Time, limit int32) ([]MediaDeleteAsset, error)
	// ListUnqualifiedDeletingAssets は、現在どの削除条件も満たさない deleting アセットを返す。
	ListUnqualifiedDeletingAssets(ctx context.Context, graceCutoff time.Time, limit int32) ([]MediaDeleteRevertCandidate, error)
	// ListTrashAssetsToDelete は、ごみ箱の猶予を過ぎたか即時削除を要求されたアセットを返す。
	ListTrashAssetsToDelete(ctx context.Context, graceCutoff time.Time, limit int32) ([]MediaDeleteAsset, error)
	// ListUntilEncodedOriginalsToDelete は、派生物が揃った until_encoded 原本を返す。
	ListUntilEncodedOriginalsToDelete(ctx context.Context, limit int32) ([]MediaDeleteAsset, error)
	// ListRemovedEncodedAssetsToDelete は、利用者が外した encoded アセットを返す。
	ListRemovedEncodedAssetsToDelete(ctx context.Context, limit int32) ([]MediaDeleteAsset, error)
	// MarkDeleting は active アセットを deleting にする。
	MarkDeleting(ctx context.Context, assetID int64) error
	// MarkUntilEncodedDeleting は削除条件を再確認して原本を deleting にする。
	// recordingID の録画に対する削除方針・作り直し要求の書き込みと直列化する。
	MarkUntilEncodedDeleting(ctx context.Context, recordingID, assetID int64) (bool, error)
	// MarkDeleted は deleting アセットを deleted にし、外した encoded の削除要求も同時に消す。
	MarkDeleted(ctx context.Context, assetID int64) error
	// RevertToActive は削除条件を再確認し、該当しなくなった deleting アセットを active に戻す。
	RevertToActive(ctx context.Context, assetID int64, graceCutoff time.Time) (bool, error)
}
