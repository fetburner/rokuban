package store

import (
	"context"
	"time"
)

// StorageSync はファイルシステムから観測したストレージ容量を表す。
type StorageSync struct {
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

// OrphanFile はファイルシステム上にあるが media_assets に無いパスを表す。
type OrphanFile struct {
	RelPath   string
	FirstSeen time.Time
}

// ActiveMediaAsset は実体無しの監査対象になる active 行を表す。
type ActiveMediaAsset struct {
	ID          int64
	RecordingID int64
	RelPath     string
	Kind        string
}

// AgedMissingMediaAsset は一定期間見つからない active 行を表す。
type AgedMissingMediaAsset struct {
	ID          int64
	RecordingID int64
	RelPath     string
	Kind        string
	FirstSeen   time.Time
}

// StorageObservation はストレージ容量と孤児・実体無し監査の永続化契約である。
type StorageObservation interface {
	// DeleteStorageSyncExcept は今回の config が要求しない root の観測行を削除する。
	DeleteStorageSyncExcept(ctx context.Context, roots []string) error
	// UpsertStorageSync は 1 root の最新観測値を保存する。
	UpsertStorageSync(ctx context.Context, observation StorageObservationInput) error
	// ListStorageSync は現在の root 別観測値を返す。
	ListStorageSync(ctx context.Context) ([]StorageSync, error)
	// ListAllMediaAssetRelPaths は孤児との差集合に使う全 media_assets パスを返す。
	ListAllMediaAssetRelPaths(ctx context.Context) ([]string, error)
	// UpsertOrphanFile は孤児候補を記録し、既存候補の first_seen を保持する。
	UpsertOrphanFile(ctx context.Context, relPath string) error
	// DeleteOrphanFile は解消した孤児候補を消す。
	DeleteOrphanFile(ctx context.Context, relPath string) error
	// ListAllOrphanFiles は記録中の孤児候補を返す。
	ListAllOrphanFiles(ctx context.Context) ([]OrphanFile, error)
	// ListAgedOrphanFiles は指定期間より前から記録されている孤児候補を返す。
	// 経過時間は候補を書いた DB の時計で比べる。
	ListAgedOrphanFiles(ctx context.Context, age time.Duration) ([]string, error)
	// ListActiveMediaAssets は実体無しの差集合に使う全 active 行を返す。
	ListActiveMediaAssets(ctx context.Context) ([]ActiveMediaAsset, error)
	// UpsertMissingMediaAsset は実体無し候補を記録し、既存候補の first_seen を保持する。
	UpsertMissingMediaAsset(ctx context.Context, mediaAssetID int64) error
	// DeleteMissingMediaAsset は実体が再び観測された候補を消す。
	DeleteMissingMediaAsset(ctx context.Context, mediaAssetID int64) error
	// ListAllMissingMediaAssetIDs は記録中の実体無し候補 ID を返す。
	ListAllMissingMediaAssetIDs(ctx context.Context) ([]int64, error)
	// ListAgedMissingMediaAssets は指定期間より前から見つからない active 行を返す。
	// 経過時間は候補を書いた DB の時計で比べる。
	ListAgedMissingMediaAssets(ctx context.Context, age time.Duration) ([]AgedMissingMediaAsset, error)
}
