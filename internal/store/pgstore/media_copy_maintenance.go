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

// ListAgedPendingMediaAssetCopies returns this store's old pending copy rows.
func (s *Media) ListAgedPendingMediaAssetCopies(ctx context.Context, before time.Time, limit int32) ([]store.MediaAssetCopy, error) {
	rows, err := sqlcgen.New(s.pool).ListAgedPendingMediaAssetCopies(ctx, sqlcgen.ListAgedPendingMediaAssetCopiesParams{
		Store: s.storeName, CreatedBefore: before, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing aged pending media asset copies: %w", err)
	}
	out := make([]store.MediaAssetCopy, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaAssetCopy{
			ID: row.ID, MediaAssetID: row.MediaAssetID, Store: row.Store, RelPath: row.RelPath, Kind: row.Kind,
		})
	}
	return out, nil
}

// ListDeletingMediaAssetCopies returns this store's copies whose DB state authorizes deletion.
func (s *Media) ListDeletingMediaAssetCopies(ctx context.Context, limit int32) ([]store.MediaAssetCopy, error) {
	rows, err := sqlcgen.New(s.pool).ListDeletingMediaAssetCopies(ctx, sqlcgen.ListDeletingMediaAssetCopiesParams{
		Store: s.storeName, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing deleting media asset copies: %w", err)
	}
	out := make([]store.MediaAssetCopy, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaAssetCopy{
			ID: row.ID, MediaAssetID: row.MediaAssetID, Store: row.Store, RelPath: row.RelPath,
			RecordingID: row.RecordingID, Kind: row.Kind, SizeBytes: row.SizeBytes,
		})
	}
	return out, nil
}

// MarkPendingMediaAssetCopyDeleted records cleanup after the pending key was removed.
func (s *Media) MarkPendingMediaAssetCopyDeleted(ctx context.Context, copyID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning pending media asset copy finalization: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	assetID, err := q.LockMediaAssetForCopyFinalizationByID(ctx, copyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("locking pending media asset copy %d for finalization: %w", copyID, err)
	}
	updated, err := q.MarkPendingMediaAssetCopyDeletedByID(ctx, copyID)
	if err != nil {
		return fmt.Errorf("marking pending media asset copy %d deleted: %w", copyID, err)
	}
	if updated > 0 {
		if err := q.FinalizeMediaAssetAfterCopyDelete(ctx, assetID); err != nil {
			return fmt.Errorf("finalizing media asset %d after pending copy deletion: %w", assetID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing pending media asset copy finalization: %w", err)
	}
	return nil
}

// ClaimAgedPendingMediaAssetCopy moves an old pending row to deleting before the object delete.
func (s *Media) ClaimAgedPendingMediaAssetCopy(ctx context.Context, copyID int64, before time.Time) (bool, error) {
	rows, err := sqlcgen.New(s.pool).ClaimAgedPendingMediaAssetCopyByID(ctx, sqlcgen.ClaimAgedPendingMediaAssetCopyByIDParams{
		ID: copyID, CreatedBefore: before,
	})
	if err != nil {
		return false, fmt.Errorf("claiming stale pending media asset copy %d: %w", copyID, err)
	}
	return rows == 1, nil
}

// MarkDeletingMediaAssetCopyDeleted records object removal and finalizes its logical asset if no copies remain.
func (s *Media) MarkDeletingMediaAssetCopyDeleted(ctx context.Context, copyID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning deleting media asset copy finalization: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	assetID, err := q.LockMediaAssetForCopyFinalizationByID(ctx, copyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("locking deleting media asset copy %d for finalization: %w", copyID, err)
	}
	updated, err := q.MarkMediaAssetCopyDeletedByID(ctx, copyID)
	if err != nil {
		return fmt.Errorf("marking deleting media asset copy %d deleted: %w", copyID, err)
	}
	if updated > 0 {
		if err := q.FinalizeMediaAssetAfterCopyDelete(ctx, assetID); err != nil {
			return fmt.Errorf("finalizing media asset %d after copy deletion: %w", assetID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing deleting media asset copy finalization: %w", err)
	}
	return nil
}
