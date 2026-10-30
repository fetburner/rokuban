package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.DerivedMediaPublisher = (*Media)(nil)
	_ store.PendingCopyReclaim    = (*Media)(nil)
)

// PreparePendingMediaAssetCopy は S3 の PUT の前に、論理行と pending のコピー行を commit する。
//
// pending は現行のコピーではない（active_media_asset_copies は active だけを返す）。
// 置き換えは新しい世代の行を足すだけで、現行の active コピーと世代を書き換えない。
// ReserveNewGeneration のときは、回収済みを含む既存のどのコピーより大きい世代だけを予約できる。
// 初回の公開で論理行が無いときだけ、予約する世代で論理行を作る。
func (s *Media) PreparePendingMediaAssetCopy(ctx context.Context, in store.PendingMediaAssetCopy) error {
	if in.Kind != store.AssetKindEncoded && in.Kind != store.AssetKindThumbnail && in.Kind != store.AssetKindSeekTiles {
		return fmt.Errorf("pending copy kind %q is not a derived media asset", in.Kind)
	}
	if in.RecordingID <= 0 || in.SizeBytes < 0 || in.DerivedGeneration < 0 {
		return errors.New("pending copy requires a positive recording ID and non-negative size and generation")
	}
	if _, err := mediapath.Resolve("/rokuban-pending-copy", in.RelPath); err != nil {
		return fmt.Errorf("validating pending media asset path: %w", err)
	}
	if (in.Kind == store.AssetKindEncoded) != (in.Profile != nil && *in.Profile != "") {
		return errors.New("encoded pending copy requires a profile; other derived kinds must not have one")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning pending media copy: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	generation := in.DerivedGeneration
	assetID, err := q.InsertMediaAssetForPendingCopy(ctx, sqlcgen.InsertMediaAssetForPendingCopyParams{
		RecordingID: in.RecordingID, Kind: in.Kind, Profile: in.Profile,
		SizeBytes: in.SizeBytes, DerivedGeneration: &generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		assetID, err = q.GetMediaAssetIDForPendingCopy(ctx, sqlcgen.GetMediaAssetIDForPendingCopyParams{
			RecordingID: in.RecordingID, Kind: in.Kind, Profile: in.Profile,
		})
	}
	if err != nil {
		return fmt.Errorf("reserving logical media asset for pending copy: %w", mapDatabaseError(err))
	}
	// 同じ論理資産への予約と公開を直列化し、世代の最大値の読みと挿入の間に別の予約が入らないようにする。
	if _, err := q.LockMediaAssetForPendingCopy(ctx, assetID); err != nil {
		return fmt.Errorf("locking media asset for generation reservation: %w", mapDatabaseError(err))
	}
	maxGeneration, err := q.GetMaxMediaAssetCopyGeneration(ctx, assetID)
	if err != nil {
		return fmt.Errorf("checking previously used generations: %w", err)
	}
	if in.DerivedGeneration < maxGeneration || (in.ReserveNewGeneration && in.DerivedGeneration == maxGeneration) {
		return fmt.Errorf("generation %d is not newer than used generation %d: %w",
			in.DerivedGeneration, maxGeneration, store.ErrGenerationAdvanced)
	}
	inserted, err := q.InsertPendingMediaAssetCopy(ctx, sqlcgen.InsertPendingMediaAssetCopyParams{
		MediaAssetID: assetID, Store: s.storeName, RelPath: in.RelPath, Generation: in.DerivedGeneration,
	})
	if err != nil {
		return fmt.Errorf("creating pending media asset copy: %w", mapDatabaseError(err))
	}
	if inserted == 0 {
		// 中断した同じ key の pending を再開する。encode は ReserveNewGeneration で上の検査が止めている。
		row, err := q.GetMediaAssetCopyByPath(ctx, sqlcgen.GetMediaAssetCopyByPathParams{Store: s.storeName, RelPath: in.RelPath})
		if err != nil {
			return fmt.Errorf("checking existing pending media asset copy: %w", mapDatabaseError(err))
		}
		if row.MediaAssetID != assetID || row.Generation != in.DerivedGeneration || row.State != "pending" {
			return fmt.Errorf("media asset copy key %q is already assigned to another asset, generation, or lifecycle state: %w", in.RelPath, store.ErrConflict)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing pending media asset copy: %w", mapDatabaseError(err))
	}
	return nil
}

// activatePendingCopy は commit の tx で pending を active にする。
// 行が回収されていれば ErrStalePublishPlan を返す。PUT した key は回収側が消す。
func activatePendingCopy(ctx context.Context, q *sqlcgen.Queries, assetID int64, storeName, relPath string, generation int64) error {
	updated, err := q.ActivatePendingMediaAssetCopy(ctx, sqlcgen.ActivatePendingMediaAssetCopyParams{
		MediaAssetID: assetID, Store: storeName, RelPath: relPath, Generation: generation,
	})
	if err != nil {
		return mapDatabaseError(err)
	}
	if updated != 1 {
		return fmt.Errorf("pending copy %q of asset %d generation %d is gone: %w", relPath, assetID, generation,
			errors.Join(store.ErrStalePublishPlan, store.ErrPendingMediaAssetCopyReclaimed))
	}
	return nil
}

// ListAgedPendingMediaAssetCopies は、before より前に予約された pending の派生物コピーを返す。
func (s *Media) ListAgedPendingMediaAssetCopies(ctx context.Context, before time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListAgedPendingMediaAssetCopies(ctx, sqlcgen.ListAgedPendingMediaAssetCopiesParams{
		StoreName: s.storeName, CreatedBefore: before, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing aged pending media asset copies: %w", err)
	}
	return mediaDeleteAssets(rows, func(row sqlcgen.ListAgedPendingMediaAssetCopiesRow) store.MediaDeleteAsset {
		return mediaDeleteAsset(row.CopyID, row.MediaAssetID, row.RecordingID, row.RelPath, row.SizeBytes, row.Kind, row.Generation)
	}), nil
}

// ReclaimPendingMediaAssetCopy は pending の行をロックしたまま deleteObject を呼び、deleted にする。
func (s *Media) ReclaimPendingMediaAssetCopy(ctx context.Context, copyID int64, before time.Time, deleteObject func() error) (bool, error) {
	if deleteObject == nil {
		return false, errors.New("reclaiming pending media asset copy: deleter is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning pending media asset copy reclaim: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	_, err = q.LockAgedPendingMediaAssetCopy(ctx, sqlcgen.LockAgedPendingMediaAssetCopyParams{
		CopyID: copyID, StoreName: s.storeName, CreatedBefore: before,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("locking pending media asset copy %d: %w", copyID, mapDatabaseError(err))
	}
	if err := deleteObject(); err != nil {
		return false, fmt.Errorf("deleting object of pending media asset copy %d: %w", copyID, err)
	}
	updated, err := q.MarkPendingMediaAssetCopyDeleted(ctx, sqlcgen.MarkPendingMediaAssetCopyDeletedParams{CopyID: copyID, StoreName: s.storeName})
	if err != nil {
		return false, fmt.Errorf("marking pending media asset copy %d deleted: %w", copyID, mapDatabaseError(err))
	}
	if updated != 1 {
		return false, fmt.Errorf("marking pending media asset copy %d deleted updated %d rows, want one", copyID, updated)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing pending media asset copy reclaim: %w", mapDatabaseError(err))
	}
	return true, nil
}
