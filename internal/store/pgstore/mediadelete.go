package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// ListPendingDeleteAssets は削除条件を満たす deleting アセットを返す。
func (s *Media) ListPendingDeleteAssets(ctx context.Context, graceCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListMediaAssetsPendingDelete(ctx, sqlcgen.ListMediaAssetsPendingDeleteParams{
		GraceCutoff: graceCutoff, StoreName: s.storeName, DesiredStore: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pending media asset deletes: %w", err)
	}
	out := make([]store.MediaDeleteAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaDeleteAsset{
			ID: row.ID, RecordingID: row.RecordingID, RelPath: row.RelPath, SizeBytes: row.SizeBytes, Kind: row.Kind,
		})
	}
	return out, nil
}

// ListUnqualifiedDeletingAssets は現在どの削除条件も満たさない deleting アセットを返す。
func (s *Media) ListUnqualifiedDeletingAssets(ctx context.Context, graceCutoff time.Time, limit int32) ([]store.MediaDeleteRevertCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListUnqualifiedDeletingAssets(ctx, sqlcgen.ListUnqualifiedDeletingAssetsParams{
		GraceCutoff: graceCutoff, StoreName: s.storeName, DesiredStore: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing unqualified deleting media assets: %w", err)
	}
	out := make([]store.MediaDeleteRevertCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaDeleteRevertCandidate{ID: row.ID, RecordingID: row.RecordingID, RelPath: row.RelPath})
	}
	return out, nil
}

// ListTrashAssetsToDelete はごみ箱の猶予を過ぎたか即時削除を要求されたアセットを返す。
func (s *Media) ListTrashAssetsToDelete(ctx context.Context, graceCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListTrashMediaAssetsToDelete(ctx, sqlcgen.ListTrashMediaAssetsToDeleteParams{
		GraceCutoff: graceCutoff, StoreName: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing trash media assets to delete: %w", err)
	}
	out := make([]store.MediaDeleteAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaDeleteAsset{
			ID: row.ID, RecordingID: row.RecordingID, RelPath: row.RelPath, SizeBytes: row.SizeBytes, Kind: row.Kind,
		})
	}
	return out, nil
}

// ListUntilEncodedOriginalsToDelete は派生物が揃った until_encoded 原本を返す。
func (s *Media) ListUntilEncodedOriginalsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListUntilEncodedOriginalsToDelete(ctx, sqlcgen.ListUntilEncodedOriginalsToDeleteParams{
		DesiredStore: s.storeName, StoreName: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing until_encoded originals to delete: %w", err)
	}
	out := make([]store.MediaDeleteAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaDeleteAsset{
			ID: row.ID, RecordingID: row.VRecordingID, RelPath: row.RelPath, SizeBytes: row.SizeBytes, Kind: row.Kind,
		})
	}
	return out, nil
}

// ListRemovedEncodedAssetsToDelete は利用者が外した encoded アセットを返す。
func (s *Media) ListRemovedEncodedAssetsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListRemovedEncodedAssetsToDelete(ctx, sqlcgen.ListRemovedEncodedAssetsToDeleteParams{
		StoreName: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing removed encoded assets to delete: %w", err)
	}
	out := make([]store.MediaDeleteAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaDeleteAsset{
			ID: row.ID, RecordingID: row.RecordingID, RelPath: row.RelPath, SizeBytes: row.SizeBytes, Kind: row.Kind,
		})
	}
	return out, nil
}

// MarkDeleting は active アセットを deleting にする。
func (s *Media) MarkDeleting(ctx context.Context, assetID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning media asset delete transition: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	updated, err := q.MarkMediaAssetDeleting(ctx, sqlcgen.MarkMediaAssetDeletingParams{
		AssetID: assetID, StoreName: s.storeName,
	})
	if err != nil {
		return fmt.Errorf("marking media asset %d deleting: %w", assetID, mapDatabaseError(err))
	}
	if updated > 0 {
		if err := setMediaAssetCopyState(ctx, q, assetID, s.storeName, "deleting"); err != nil {
			return fmt.Errorf("marking media asset copy %d deleting: %w", assetID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing media asset delete transition: %w", mapDatabaseError(err))
	}
	return nil
}

// MarkUntilEncodedDeleting は録画の削除方針と作り直し要求に直列化して削除条件を再確認する。
func (s *Media) MarkUntilEncodedDeleting(ctx context.Context, recordingID, assetID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning until_encoded media asset delete for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if err := lockRecording(ctx, q, recordingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("locking recording %d before until_encoded delete: %w", recordingID, err)
	}
	marked, err := q.MarkUntilEncodedOriginalDeleting(ctx, sqlcgen.MarkUntilEncodedOriginalDeletingParams{
		AssetID: assetID, StoreName: s.storeName, DesiredStore: s.storeName,
	})
	if err != nil {
		return false, fmt.Errorf("marking until_encoded original %d deleting: %w", assetID, mapDatabaseError(err))
	}
	if marked == 0 {
		return false, nil
	}
	if err := setMediaAssetCopyState(ctx, q, assetID, s.storeName, "deleting"); err != nil {
		return false, fmt.Errorf("marking until_encoded media asset copy %d deleting: %w", assetID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing until_encoded delete for media asset %d: %w", assetID, mapDatabaseError(err))
	}
	return true, nil
}

// MarkDeleted は deleting アセットを deleted にし、外した encoded の削除要求も同時に消す。
func (s *Media) MarkDeleted(ctx context.Context, assetID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning media asset delete completion: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	updated, err := q.MarkMediaAssetDeleted(ctx, sqlcgen.MarkMediaAssetDeletedParams{
		ID: assetID, StoreName: s.storeName,
	})
	if err != nil {
		return fmt.Errorf("marking media asset %d deleted: %w", assetID, mapDatabaseError(err))
	}
	if updated > 0 {
		if err := setMediaAssetCopyState(ctx, q, assetID, s.storeName, "deleted"); err != nil {
			return fmt.Errorf("marking media asset copy %d deleted: %w", assetID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing media asset delete completion: %w", mapDatabaseError(err))
	}
	return nil
}

// RevertToActive は削除条件から外れた deleting アセットを active に戻す。
func (s *Media) RevertToActive(ctx context.Context, assetID int64, graceCutoff time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning media asset restore transition: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	updated, err := q.RevertMediaAssetToActive(ctx, sqlcgen.RevertMediaAssetToActiveParams{
		ID: assetID, GraceCutoff: graceCutoff, StoreName: s.storeName, DesiredStore: s.storeName,
	})
	if err != nil {
		return false, fmt.Errorf("reverting media asset %d to active: %w", assetID, mapDatabaseError(err))
	}
	if updated > 0 {
		if err := setMediaAssetCopyState(ctx, q, assetID, s.storeName, "active"); err != nil {
			return false, fmt.Errorf("reverting media asset copy %d to active: %w", assetID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing media asset restore transition: %w", mapDatabaseError(err))
	}
	return updated > 0, nil
}

func setMediaAssetCopyState(ctx context.Context, q *sqlcgen.Queries, assetID int64, storeName, state string) error {
	updated, err := q.SetMediaAssetCopyState(ctx, sqlcgen.SetMediaAssetCopyStateParams{
		MediaAssetID: assetID, Store: storeName, State: state,
	})
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("updated %d rows, want exactly one copy", updated)
	}
	return nil
}
