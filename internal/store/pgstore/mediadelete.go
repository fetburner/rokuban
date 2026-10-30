package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// ListPendingDeleteAssets は削除条件を満たす deleting コピーを返す。
func (s *Media) ListPendingDeleteAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListMediaAssetsPendingDelete(ctx, sqlcgen.ListMediaAssetsPendingDeleteParams{
		GraceCutoff: graceCutoff, OverlapCutoff: overlapCutoff, StoreName: s.storeName,
		DesiredStore: mediaStoreNameArg(s.derivedStore), OriginalRules: originalRules, DerivedStore: derivedStore, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pending media asset deletes: %w", err)
	}
	return mediaDeleteAssets(rows, func(row sqlcgen.ListMediaAssetsPendingDeleteRow) store.MediaDeleteAsset {
		return mediaDeleteAsset(row.CopyID, row.MediaAssetID, row.RecordingID, row.RelPath, row.SizeBytes, row.Kind, row.Generation)
	}), nil
}

// ListUnqualifiedDeletingAssets は現在どの削除条件も満たさない deleting コピーを返す。
func (s *Media) ListUnqualifiedDeletingAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string, limit int32) ([]store.MediaDeleteRevertCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListUnqualifiedDeletingAssets(ctx, sqlcgen.ListUnqualifiedDeletingAssetsParams{
		GraceCutoff: graceCutoff, OverlapCutoff: overlapCutoff, StoreName: s.storeName,
		DesiredStore: mediaStoreNameArg(s.derivedStore), OriginalRules: originalRules, DerivedStore: derivedStore, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing unqualified deleting media asset copies: %w", err)
	}
	out := make([]store.MediaDeleteRevertCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaDeleteRevertCandidate{
			ID: row.CopyID, MediaAssetID: row.MediaAssetID, RecordingID: row.RecordingID, RelPath: row.RelPath,
		})
	}
	return out, nil
}

// ListOldGenerationMediaAssetCopiesToDelete は overlap を過ぎた旧世代コピーを返す。
func (s *Media) ListOldGenerationMediaAssetCopiesToDelete(ctx context.Context, overlapCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListOldGenerationMediaAssetCopiesToDelete(ctx, sqlcgen.ListOldGenerationMediaAssetCopiesToDeleteParams{
		StoreName: s.storeName, OverlapCutoff: overlapCutoff, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing old-generation media asset copies: %w", err)
	}
	return mediaDeleteAssets(rows, func(row sqlcgen.ListOldGenerationMediaAssetCopiesToDeleteRow) store.MediaDeleteAsset {
		return mediaDeleteAsset(row.CopyID, row.MediaAssetID, row.RecordingID, row.RelPath, row.SizeBytes, row.Kind, row.Generation)
	}), nil
}

// ListTrashAssetsToDelete はごみ箱の猶予を過ぎたか即時削除を要求されたコピーを返す。
func (s *Media) ListTrashAssetsToDelete(ctx context.Context, graceCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListTrashMediaAssetsToDelete(ctx, sqlcgen.ListTrashMediaAssetsToDeleteParams{
		GraceCutoff: graceCutoff, StoreName: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing trash media asset copies to delete: %w", err)
	}
	return mediaDeleteAssets(rows, func(row sqlcgen.ListTrashMediaAssetsToDeleteRow) store.MediaDeleteAsset {
		return mediaDeleteAsset(row.CopyID, row.MediaAssetID, row.RecordingID, row.RelPath, row.SizeBytes, row.Kind, row.Generation)
	}), nil
}

// ListUntilEncodedOriginalsToDelete は派生物が揃った until_encoded 原本コピーを返す。
func (s *Media) ListUntilEncodedOriginalsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListUntilEncodedOriginalsToDelete(ctx, sqlcgen.ListUntilEncodedOriginalsToDeleteParams{
		DesiredStore: mediaStoreNameArg(s.derivedStore), StoreName: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing until_encoded original copies to delete: %w", err)
	}
	return mediaDeleteAssets(rows, func(row sqlcgen.ListUntilEncodedOriginalsToDeleteRow) store.MediaDeleteAsset {
		return mediaDeleteAsset(row.CopyID, row.MediaAssetID, row.VRecordingID, row.RelPath, row.SizeBytes, row.Kind, row.Generation)
	}), nil
}

// ListRemovedEncodedAssetsToDelete は利用者が外した encoded コピーを返す。
func (s *Media) ListRemovedEncodedAssetsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListRemovedEncodedAssetsToDelete(ctx, sqlcgen.ListRemovedEncodedAssetsToDeleteParams{
		StoreName: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing removed encoded asset copies to delete: %w", err)
	}
	return mediaDeleteAssets(rows, func(row sqlcgen.ListRemovedEncodedAssetsToDeleteRow) store.MediaDeleteAsset {
		return mediaDeleteAsset(row.CopyID, row.MediaAssetID, row.RecordingID, row.RelPath, row.SizeBytes, row.Kind, row.Generation)
	}), nil
}

func mediaDeleteAsset(copyID, mediaAssetID, recordingID int64, relPath string, sizeBytes int64, kind string, generation int64) store.MediaDeleteAsset {
	return store.MediaDeleteAsset{
		ID: copyID, MediaAssetID: mediaAssetID, RecordingID: recordingID,
		RelPath: relPath, SizeBytes: sizeBytes, Kind: kind, Generation: generation,
	}
}

func mediaDeleteAssets[T any](rows []T, convert func(T) store.MediaDeleteAsset) []store.MediaDeleteAsset {
	out := make([]store.MediaDeleteAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, convert(row))
	}
	return out
}

// MarkDeleting は active コピーを deleting にする。
func (s *Media) MarkDeleting(ctx context.Context, copyID int64, expectedState string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning media asset copy delete transition: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	copy, err := lockMediaAssetCopy(ctx, q, copyID, s.storeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if copy.State != expectedState {
		return false, nil
	}
	updated, err := q.SetMediaAssetCopyState(ctx, sqlcgen.SetMediaAssetCopyStateParams{
		ID: copyID, StoreName: s.storeName, State: "deleting", ExpectedState: expectedState,
	})
	if err != nil {
		return false, fmt.Errorf("marking media asset copy %d deleting: %w", copyID, mapDatabaseError(err))
	}
	if updated != 1 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing media asset copy delete transition: %w", mapDatabaseError(err))
	}
	return true, nil
}

// MarkUntilEncodedDeleting は録画の削除方針と作り直し要求に直列化して削除条件を再確認する。
func (s *Media) MarkUntilEncodedDeleting(ctx context.Context, recordingID, mediaAssetID, copyID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning until_encoded media asset copy delete for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := lockRecording(ctx, q, recordingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("locking recording %d before until_encoded delete: %w", recordingID, err)
	}
	copy, err := lockMediaAssetCopy(ctx, q, copyID, s.storeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if copy.MediaAssetID != mediaAssetID || copy.State != "active" {
		return false, nil
	}
	marked, err := q.MarkUntilEncodedOriginalCopyDeleting(ctx, sqlcgen.MarkUntilEncodedOriginalCopyDeletingParams{
		AssetID: mediaAssetID, CopyID: copyID, StoreName: s.storeName, DesiredStore: mediaStoreNameArg(s.derivedStore),
	})
	if err != nil {
		return false, fmt.Errorf("marking until_encoded original copy %d deleting: %w", copyID, mapDatabaseError(err))
	}
	if marked != 1 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing until_encoded delete for media asset copy %d: %w", copyID, mapDatabaseError(err))
	}
	return true, nil
}

// MarkDeleted は deleting コピーを deleted にし、全コピーの削除後に encoded 削除要求を消す。
func (s *Media) MarkDeleted(ctx context.Context, copyID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning media asset copy delete completion: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	copy, err := lockMediaAssetCopy(ctx, q, copyID, s.storeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if copy.State != "deleted" {
		if copy.State != "deleting" {
			return fmt.Errorf("media asset copy %d has state %q, want deleting or deleted", copyID, copy.State)
		}
		updated, err := q.MarkMediaAssetCopyDeleted(ctx, sqlcgen.MarkMediaAssetCopyDeletedParams{
			CopyID: copyID, StoreName: s.storeName,
		})
		if err != nil {
			return fmt.Errorf("marking media asset copy %d deleted: %w", copyID, mapDatabaseError(err))
		}
		if updated != 1 {
			return fmt.Errorf("marking media asset copy %d deleted updated %d rows, want one", copyID, updated)
		}
	}
	if err := q.ClearDeletedEncodedAssetRemovalRequest(ctx, copyID); err != nil {
		return fmt.Errorf("clearing completed encoded asset removal request for copy %d: %w", copyID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing media asset copy delete completion: %w", mapDatabaseError(err))
	}
	return nil
}

// RevertToActive は削除条件から外れた deleting コピーを active に戻す。
func (s *Media) RevertToActive(ctx context.Context, copyID int64, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning media asset copy restore transition: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	copy, err := lockMediaAssetCopy(ctx, q, copyID, s.storeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if copy.State != "deleting" {
		return false, nil
	}
	updated, err := q.RevertMediaAssetCopyToActive(ctx, sqlcgen.RevertMediaAssetCopyToActiveParams{
		CopyID: copyID, GraceCutoff: graceCutoff, OverlapCutoff: overlapCutoff,
		StoreName: s.storeName, DesiredStore: mediaStoreNameArg(s.derivedStore),
		OriginalRules: originalRules, DerivedStore: derivedStore,
	})
	if err != nil {
		return false, fmt.Errorf("reverting media asset copy %d to active: %w", copyID, mapDatabaseError(err))
	}
	if updated == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing media asset copy restore transition: %w", mapDatabaseError(err))
	}
	return true, nil
}

type lockedMediaAssetCopy struct {
	MediaAssetID int64
	State        string
}

// lockMediaAssetCopy は物理コピーの状態を読み、同じトランザクション内で遷移させる。
func lockMediaAssetCopy(ctx context.Context, q *sqlcgen.Queries, copyID int64, storeName string) (lockedMediaAssetCopy, error) {
	row, err := q.LockMediaAssetCopy(ctx, sqlcgen.LockMediaAssetCopyParams{CopyID: copyID, StoreName: storeName})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return lockedMediaAssetCopy{}, store.ErrNotFound
		}
		return lockedMediaAssetCopy{}, fmt.Errorf("locking media asset copy %d: %w", copyID, mapDatabaseError(err))
	}
	return lockedMediaAssetCopy{MediaAssetID: row.MediaAssetID, State: row.State}, nil
}
