package store

import "context"

// MediaAssetTransferCandidate は desired store にコピーが必要な論理資産を表す。
type MediaAssetTransferCandidate struct {
	MediaAssetID int64
	Store        string
}

// MediaAssetTransfer は 1 回の転送に必要な、ロック後の論理資産とコピーの状態を表す。
type MediaAssetTransfer struct {
	MediaAssetID  int64
	RecordingID   int64
	Generation    int64
	RelPath       string
	SizeBytes     int64
	Kind          string
	SHA256        []byte
	DesiredStore  string
	SourceStore   string
	SourceRelPath string
	SourceCopyID  int64
	CopyID        int64
}

// MediaAssetCopyDeleteCandidate は配置規則から外れた物理コピーの削除候補を表す。
type MediaAssetCopyDeleteCandidate struct {
	CopyID       int64
	MediaAssetID int64
	Store        string
	RelPath      string
	State        string
	SizeBytes    int64
	Kind         string
	Generation   int64
}

// TransferBandwidthLease は転送元置き場の排他を保持する専用 DB 接続を表す。
type TransferBandwidthLease interface {
	// Release は advisory lock を解放し、専用接続を閉じる。
	Release(ctx context.Context) error
}

// StorageTransfer は desired state と物理コピーの reconcile を永続化する契約である。
type StorageTransfer interface {
	// ListMediaAssetTransferCandidates は active で desired store にコピーが無い資産を返す。
	ListMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit, limit int32) ([]MediaAssetTransferCandidate, error)
	// CountMediaAssetTransferCandidates は現在の転送待ちの件数とバイト数を返す。
	CountMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (assetCount, byteCount int64, err error)
	// CountMediaAssetTransferExhausted は失敗上限に達して reconcile から外れている件数を返す。
	CountMediaAssetTransferExhausted(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (int64, error)
	// ClearObsoleteMediaAssetTransferAttempts は現在の desired / generation から外れた失敗行を消す。
	ClearObsoleteMediaAssetTransferAttempts(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string) error
	// AcquireTransferSourceBandwidth は転送元置き場ごとの advisory lock を取る。
	AcquireTransferSourceBandwidth(ctx context.Context, sourceStore string) (lease TransferBandwidthLease, acquired bool, err error)
	// PrepareMediaAssetTransfer は行をロックし、現在も desired な宛先コピーを pending にする。
	PrepareMediaAssetTransfer(ctx context.Context, mediaAssetID int64, targetStore string, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (MediaAssetTransfer, bool, error)
	// CompleteMediaAssetTransfer は SHA-256 を観測し、現在も desired な pending コピーを active にする。
	CompleteMediaAssetTransfer(ctx context.Context, transfer MediaAssetTransfer, targetStore, derivedStore string, originalRules, sha256 []byte) (activated bool, rejection string, err error)
	// RecordMediaAssetTransferFailure は転送失敗を記録する。
	RecordMediaAssetTransferFailure(ctx context.Context, copyID int64, message string) error
	// MarkMediaAssetCopyCorrupt は破損が確認されたコピーを deleted にする。
	MarkMediaAssetCopyCorrupt(ctx context.Context, mediaAssetID, copyID int64, storeName string) (bool, error)
	// DiscardPendingMediaAssetCopy は失敗した転送先コピーを deleted にする。
	DiscardPendingMediaAssetCopy(ctx context.Context, copyID int64) error
	// ListMediaAssetCopiesToDelete は、現行 policy の desired copy を確認できる stale copy を返す。
	ListMediaAssetCopiesToDelete(ctx context.Context, originalRules []byte, derivedStore string, overlapMicros int64, limit int32) ([]MediaAssetCopyDeleteCandidate, error)
	// GetMediaAssetCopyForDeletion はジョブ引数の copy ID から現在のパスを返す。
	GetMediaAssetCopyForDeletion(ctx context.Context, candidate MediaAssetCopyDeleteCandidate) (MediaAssetCopyDeleteCandidate, bool, error)
	// MarkMediaAssetCopyDeleting は media_assets をロックし直し、削除条件を再評価して path を返す。
	MarkMediaAssetCopyDeleting(ctx context.Context, candidate MediaAssetCopyDeleteCandidate, originalRules []byte, derivedStore string, overlapMicros int64) (relPath string, marked bool, err error)
	// MarkMediaAssetCopyDeleted は unlink 済みの deleting copy を deleted にする。
	MarkMediaAssetCopyDeleted(ctx context.Context, candidate MediaAssetCopyDeleteCandidate) (bool, error)
}
