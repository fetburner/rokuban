package store

import (
	"context"
	"time"
)

// StorageSync はファイルシステムから観測したストレージ容量を表す。
type StorageSync struct {
	// Store は観測した置き場の名前（storage_sync.store）。
	Store          string
	Root           string
	Path           string
	TotalBytes     int64
	UsedBytes      int64
	AvailableBytes int64
	ObservedAt     time.Time
}

// StorageObservationInput は storage_sync に保存する 1 root の観測値を表す。
type StorageObservationInput struct {
	Root           string
	Path           string
	TotalBytes     int64
	UsedBytes      int64
	AvailableBytes int64
}

// OrphanFile はファイルシステム上にあるが、この置き場の media_asset_copies に無いパスを表す。
type OrphanFile struct {
	RelPath   string
	FirstSeen time.Time
}

// ActiveMediaAsset は実体無しの監査対象になる active コピーと論理資産の属性を表す。
type ActiveMediaAsset struct {
	// ID は物理コピー行の ID。
	ID           int64
	MediaAssetID int64
	RecordingID  int64
	RelPath      string
	Kind         string
}

// AgedMissingMediaAsset は一定期間見つからない active コピーを表す。
type AgedMissingMediaAsset struct {
	// ID は物理コピー行の ID。
	ID           int64
	MediaAssetID int64
	RecordingID  int64
	RelPath      string
	Kind         string
	FirstSeen    time.Time
}

// StorageObservation はストレージ容量と孤児・実体無し監査の永続化契約である。
type StorageObservation interface {
	// DeleteStorageSyncExcept は今回の config が要求しない root の観測行を削除する。
	DeleteStorageSyncExcept(ctx context.Context, roots []string) error
	// UpsertStorageSync は 1 root の最新観測値を保存する。
	UpsertStorageSync(ctx context.Context, observation StorageObservationInput) error
	// ListStorageSync は現在の root 別観測値を返す。
	ListStorageSync(ctx context.Context) ([]StorageSync, error)
	// ListAllMediaAssetRelPaths は孤児との差集合に使う、この置き場の全コピーのパスを返す。
	ListAllMediaAssetRelPaths(ctx context.Context) ([]string, error)
	// UpsertOrphanFile は孤児候補を記録し、既存候補の first_seen を保持する。
	UpsertOrphanFile(ctx context.Context, relPath string) error
	// DeleteOrphanFile は解消した孤児候補を消す。
	DeleteOrphanFile(ctx context.Context, relPath string) error
	// GetMediaAssetTransferProgressGeneration は、コピーの転送の進捗が今指す世代を返す。行が無ければ ok=false。
	// 孤児の転送 temp が引き継がれないかを、世代で判定するために使う。
	GetMediaAssetTransferProgressGeneration(ctx context.Context, copyID int64) (generation int64, ok bool, err error)
	// ListAllOrphanFiles は記録中の孤児候補を返す。
	ListAllOrphanFiles(ctx context.Context) ([]OrphanFile, error)
	// ListAgedOrphanFiles は指定期間より前から記録されている孤児候補を返す。
	// 経過時間は候補を書いた DB の時計で比べる。
	ListAgedOrphanFiles(ctx context.Context, age time.Duration) ([]string, error)
	// ListActiveMediaAssets は実体無しの差集合に使う、この置き場の全 active コピーを返す。
	ListActiveMediaAssets(ctx context.Context) ([]ActiveMediaAsset, error)
	// UpsertMissingMediaAsset は実体無し候補を記録し、既存候補の first_seen を保持する。
	UpsertMissingMediaAsset(ctx context.Context, copyID int64) error
	// DeleteMissingMediaAsset は実体が再び観測された候補を消す。
	DeleteMissingMediaAsset(ctx context.Context, copyID int64) error
	// ListAllMissingMediaAssetIDs は記録中の実体無し候補 ID を返す。
	ListAllMissingMediaAssetIDs(ctx context.Context) ([]int64, error)
	// ListAgedMissingMediaAssets は指定期間より前から見つからない active コピーを返す。
	// 経過時間は候補を書いた DB の時計で比べる。
	ListAgedMissingMediaAssets(ctx context.Context, age time.Duration) ([]AgedMissingMediaAsset, error)
}
