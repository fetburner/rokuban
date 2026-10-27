package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// ListMediaAssetTransferCandidates は desired store にコピーが無い論理資産を列挙する。
func (s *Media) ListMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit, limit int32) ([]store.MediaAssetTransferCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListMediaAssetTransferCandidates(ctx, sqlcgen.ListMediaAssetTransferCandidatesParams{
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore, AttemptLimit: attemptLimit, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing media asset transfer candidates: %w", err)
	}
	out := make([]store.MediaAssetTransferCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaAssetTransferCandidate{MediaAssetID: row.MediaAssetID, Store: row.DesiredStore})
	}
	return out, nil
}

// CountMediaAssetTransferCandidates は転送待ちの件数とバイト数を返す。
func (s *Media) CountMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (int64, int64, error) {
	row, err := sqlcgen.New(s.pool).CountMediaAssetTransferCandidates(ctx, sqlcgen.CountMediaAssetTransferCandidatesParams{
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore, AttemptLimit: attemptLimit,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("counting media asset transfer candidates: %w", err)
	}
	return row.AssetCount, row.ByteCount, nil
}

// PrepareMediaAssetTransfer は現在も必要なコピーだけを pending に予約して返す。
func (s *Media) PrepareMediaAssetTransfer(ctx context.Context, mediaAssetID int64, targetStore string, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (store.MediaAssetTransfer, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("beginning media asset transfer reservation: %w", err)
	}
	defer rollback(tx)
	row, err := sqlcgen.New(tx).PrepareMediaAssetTransfer(ctx, sqlcgen.PrepareMediaAssetTransferParams{
		MediaAssetID: mediaAssetID, TargetStore: targetStore, OriginalRules: originalRules, FsStores: fsStores,
		DerivedStore: derivedStore, AttemptLimit: attemptLimit,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MediaAssetTransfer{}, false, nil
	}
	if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("preparing media asset %d transfer to %q: %w", mediaAssetID, targetStore, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("committing media asset transfer reservation: %w", mapDatabaseError(err))
	}
	return store.MediaAssetTransfer{
		MediaAssetID: row.MediaAssetID, RecordingID: row.RecordingID, RelPath: row.RelPath,
		SizeBytes: row.SizeBytes, Kind: row.Kind, SHA256: row.Sha256, DesiredStore: row.DesiredStore,
		SourceStore: row.SourceStore, SourceRelPath: row.SourceRelPath, SourceCopyID: row.SourceCopyID,
		CopyID: row.CopyID, PreviousCopyState: row.PreviousCopyState,
	}, true, nil
}

// CompleteMediaAssetTransfer はファイル公開後に desired policy を再評価してコピーを active にする。
func (s *Media) CompleteMediaAssetTransfer(ctx context.Context, transfer store.MediaAssetTransfer, targetStore, derivedStore string, originalRules, sha256 []byte) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning media asset transfer completion: %w", err)
	}
	defer rollback(tx)
	count, err := sqlcgen.New(tx).CompleteMediaAssetTransfer(ctx, sqlcgen.CompleteMediaAssetTransferParams{
		Sha256: sha256, MediaAssetID: transfer.MediaAssetID, OriginalRules: originalRules,
		DerivedStore: derivedStore, TargetStore: targetStore, SizeBytes: transfer.SizeBytes, CopyID: transfer.CopyID,
	})
	if err != nil {
		return false, fmt.Errorf("activating media asset %d copy in %q: %w", transfer.MediaAssetID, targetStore, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing media asset transfer: %w", mapDatabaseError(err))
	}
	return count == 1, nil
}

// RecordMediaAssetTransferFailure は転送失敗を宛先ごとに記録する。
func (s *Media) RecordMediaAssetTransferFailure(ctx context.Context, mediaAssetID int64, targetStore, message string) error {
	if err := sqlcgen.New(s.pool).RecordMediaAssetTransferFailure(ctx, sqlcgen.RecordMediaAssetTransferFailureParams{
		MediaAssetID: mediaAssetID, Store: targetStore, LastError: message,
	}); err != nil {
		return fmt.Errorf("recording media asset %d transfer failure for %q: %w", mediaAssetID, targetStore, err)
	}
	return nil
}

// MarkMediaAssetCopyCorrupt は照合で破損が分かったコピーを deleted にする。
func (s *Media) MarkMediaAssetCopyCorrupt(ctx context.Context, mediaAssetID, copyID int64, storeName string) error {
	if err := sqlcgen.New(s.pool).MarkMediaAssetCopyCorrupt(ctx, sqlcgen.MarkMediaAssetCopyCorruptParams{
		CopyID: copyID, MediaAssetID: mediaAssetID, Store: storeName,
	}); err != nil {
		return fmt.Errorf("marking corrupt media asset %d copy in %q: %w", mediaAssetID, storeName, err)
	}
	return nil
}

// DiscardPendingMediaAssetCopy は pending な転送先コピーを無効にする。
func (s *Media) DiscardPendingMediaAssetCopy(ctx context.Context, mediaAssetID int64, storeName string) error {
	if err := sqlcgen.New(s.pool).DiscardPendingMediaAssetCopy(ctx, sqlcgen.DiscardPendingMediaAssetCopyParams{
		MediaAssetID: mediaAssetID, Store: storeName,
	}); err != nil {
		return fmt.Errorf("discarding pending media asset %d copy in %q: %w", mediaAssetID, storeName, err)
	}
	return nil
}

// ListMediaAssetCopiesToDelete は overlap を過ぎ、現行 policy で削除可能なコピーを列挙する。
func (s *Media) ListMediaAssetCopiesToDelete(ctx context.Context, originalRules []byte, derivedStore string, overlapMicros int64, limit int32) ([]store.MediaAssetCopyDeleteCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListMediaAssetCopiesToDelete(ctx, sqlcgen.ListMediaAssetCopiesToDeleteParams{
		OriginalRules: originalRules, DerivedStore: derivedStore, OverlapUs: overlapMicros, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing stale media asset copies: %w", err)
	}
	out := make([]store.MediaAssetCopyDeleteCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaAssetCopyDeleteCandidate{
			CopyID: row.CopyID, MediaAssetID: row.MediaAssetID, Store: row.Store,
			RelPath: row.RelPath, SizeBytes: row.SizeBytes, Kind: row.Kind,
		})
	}
	return out, nil
}

// GetMediaAssetCopyForDeletion はジョブ引数の copy ID から現在のコピー状態を返す。
func (s *Media) GetMediaAssetCopyForDeletion(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate) (store.MediaAssetCopyDeleteCandidate, bool, error) {
	row, err := sqlcgen.New(s.pool).GetMediaAssetCopyForDeletion(ctx, sqlcgen.GetMediaAssetCopyForDeletionParams{
		CopyID: candidate.CopyID, MediaAssetID: candidate.MediaAssetID, Store: candidate.Store,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MediaAssetCopyDeleteCandidate{}, false, nil
	}
	if err != nil {
		return store.MediaAssetCopyDeleteCandidate{}, false, fmt.Errorf("getting media asset %d copy in %q for deletion: %w", candidate.MediaAssetID, candidate.Store, err)
	}
	return store.MediaAssetCopyDeleteCandidate{
		CopyID: row.CopyID, MediaAssetID: row.MediaAssetID, Store: row.Store,
		RelPath: row.RelPath, SizeBytes: row.SizeBytes, Kind: row.Kind,
	}, true, nil
}

// MarkMediaAssetCopyDeleting は論理行を先にロックし、その後の SQL 文で削除条件を再評価する。
func (s *Media) MarkMediaAssetCopyDeleting(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate, originalRules []byte, derivedStore string, overlapMicros int64) (string, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("beginning media asset copy delete: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if _, err := q.LockMediaAssetForStorageTransfer(ctx, candidate.MediaAssetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("locking logical media asset %d before copy deletion: %w", candidate.MediaAssetID, err)
	}
	path, err := q.MarkMediaAssetCopyDeleting(ctx, sqlcgen.MarkMediaAssetCopyDeletingParams{
		CopyID: candidate.CopyID, MediaAssetID: candidate.MediaAssetID, Store: candidate.Store,
		OriginalRules: originalRules, DerivedStore: derivedStore, OverlapUs: overlapMicros,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("marking media asset %d copy in %q deleting: %w", candidate.MediaAssetID, candidate.Store, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("committing media asset copy delete mark: %w", mapDatabaseError(err))
	}
	return path, true, nil
}

// MarkMediaAssetCopyDeleted は unlink 済みの deleting copy を確定する。
func (s *Media) MarkMediaAssetCopyDeleted(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate) (bool, error) {
	count, err := sqlcgen.New(s.pool).MarkMediaAssetCopyDeleted(ctx, sqlcgen.MarkMediaAssetCopyDeletedParams{
		CopyID: candidate.CopyID, MediaAssetID: candidate.MediaAssetID, Store: candidate.Store,
	})
	if err != nil {
		return false, fmt.Errorf("marking media asset %d copy in %q deleted: %w", candidate.MediaAssetID, candidate.Store, err)
	}
	return count == 1, nil
}
