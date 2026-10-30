package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// ListMediaAssetTransferCandidates は desired store にコピーが無い論理資産を列挙する。
func (s *Media) ListMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit, limit int32) ([]store.MediaAssetTransferCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListMediaAssetTransferCandidates(ctx, sqlcgen.ListMediaAssetTransferCandidatesParams{
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore, AttemptLimit: int64(attemptLimit), RowLimit: limit,
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
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore, AttemptLimit: int64(attemptLimit),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("counting media asset transfer candidates: %w", err)
	}
	return row.AssetCount, row.ByteCount, nil
}

// CountMediaAssetTransferExhausted は失敗上限に達した転送候補を数える。
func (s *Media) CountMediaAssetTransferExhausted(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (int64, error) {
	count, err := sqlcgen.New(s.pool).CountMediaAssetTransferExhausted(ctx, sqlcgen.CountMediaAssetTransferExhaustedParams{
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore, AttemptLimit: int64(attemptLimit),
	})
	if err != nil {
		return 0, fmt.Errorf("counting exhausted media asset transfers: %w", err)
	}
	return count, nil
}

// ClearObsoleteMediaAssetTransferAttempts は現行 desired でない失敗行を削除する。
func (s *Media) ClearObsoleteMediaAssetTransferAttempts(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string) error {
	if err := sqlcgen.New(s.pool).ClearObsoleteMediaAssetTransferAttempts(ctx, sqlcgen.ClearObsoleteMediaAssetTransferAttemptsParams{
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore,
	}); err != nil {
		return fmt.Errorf("clearing obsolete media asset transfer attempts: %w", err)
	}
	return nil
}

// AcquireTransferSourceBandwidth は転送元置き場の advisory lock を専用接続で保持する。
func (s *Media) AcquireTransferSourceBandwidth(ctx context.Context, sourceStore string) (store.TransferBandwidthLease, bool, error) {
	if sourceStore == "" {
		return nil, false, errors.New("transfer source store is empty")
	}
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring transfer bandwidth connection: %w", err)
	}
	conn := pooled.Hijack()
	key := "rokuban-storage-transfer:" + sourceStore
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&acquired); err != nil {
		closeErr := conn.Close(context.Background())
		return nil, false, errors.Join(fmt.Errorf("acquiring transfer bandwidth lock for %q: %w", sourceStore, err), closeErr)
	}
	if !acquired {
		if err := conn.Close(ctx); err != nil {
			return nil, false, fmt.Errorf("closing unused transfer bandwidth connection: %w", err)
		}
		return nil, false, nil
	}
	return &transferBandwidthLease{conn: conn, key: key}, true, nil
}

type transferBandwidthLease struct {
	conn *pgx.Conn
	key  string
	once sync.Once
	err  error
}

func (l *transferBandwidthLease) Release(ctx context.Context) error {
	l.once.Do(func() {
		var unlocked bool
		unlockErr := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.key).Scan(&unlocked)
		if unlockErr == nil && !unlocked {
			unlockErr = errors.New("transfer bandwidth advisory lock was not held")
		}
		closeErr := l.conn.Close(ctx)
		l.err = errors.Join(unlockErr, closeErr)
	})
	return l.err
}

var _ store.TransferBandwidthLease = (*transferBandwidthLease)(nil)

// PrepareMediaAssetTransfer は現在も必要なコピーだけを pending に予約して返す。
func (s *Media) PrepareMediaAssetTransfer(ctx context.Context, mediaAssetID int64, targetStore string, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (store.MediaAssetTransfer, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("beginning media asset transfer reservation: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if _, err := q.LockMediaAssetForStorageTransfer(ctx, mediaAssetID); errors.Is(err, pgx.ErrNoRows) {
		return store.MediaAssetTransfer{}, false, nil
	} else if err != nil {
		return store.MediaAssetTransfer{}, false, fmt.Errorf("locking media asset %d before transfer reservation: %w", mediaAssetID, err)
	}
	row, err := q.PrepareMediaAssetTransfer(ctx, sqlcgen.PrepareMediaAssetTransferParams{
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
		MediaAssetID: row.MediaAssetID, RecordingID: row.RecordingID, Generation: row.Generation, RelPath: row.RelPath,
		SizeBytes: row.SizeBytes, Kind: row.Kind, SHA256: row.Sha256, DesiredStore: row.DesiredStore,
		SourceStore: row.SourceStore, SourceRelPath: row.SourceRelPath, SourceCopyID: row.SourceCopyID,
		CopyID: row.CopyID,
	}, true, nil
}

// CompleteMediaAssetTransfer はファイル公開後に desired policy を再評価してコピーを active にする。
func (s *Media) CompleteMediaAssetTransfer(ctx context.Context, transfer store.MediaAssetTransfer, targetStore, derivedStore string, originalRules, sha256 []byte) (bool, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, "", fmt.Errorf("beginning media asset transfer completion: %w", err)
	}
	defer rollback(tx)
	generation := transfer.Generation
	outcome, err := sqlcgen.New(tx).CompleteMediaAssetTransfer(ctx, sqlcgen.CompleteMediaAssetTransferParams{
		Sha256: sha256, MediaAssetID: transfer.MediaAssetID, OriginalRules: originalRules,
		DerivedStore: derivedStore, TargetStore: targetStore, SizeBytes: transfer.SizeBytes,
		Generation: &generation, CopyID: transfer.CopyID,
	})
	if err != nil {
		return false, "", fmt.Errorf("activating media asset %d copy in %q: %w", transfer.MediaAssetID, targetStore, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, "", fmt.Errorf("committing media asset transfer: %w", mapDatabaseError(err))
	}
	return outcome == "activated" || outcome == "already_active", outcome, nil
}

// RecordMediaAssetTransferFailure は転送失敗を宛先ごとに記録する。
func (s *Media) RecordMediaAssetTransferFailure(ctx context.Context, copyID int64, message string) error {
	if err := sqlcgen.New(s.pool).RecordMediaAssetTransferFailure(ctx, sqlcgen.RecordMediaAssetTransferFailureParams{
		CopyID: copyID, LastError: message,
	}); err != nil {
		return fmt.Errorf("recording transfer failure for media asset copy %d: %w", copyID, err)
	}
	return nil
}

// MarkMediaAssetCopyCorrupt は照合で破損が分かったコピーを deleted にする。
func (s *Media) MarkMediaAssetCopyCorrupt(ctx context.Context, mediaAssetID, copyID int64, storeName string) (bool, error) {
	updated, err := sqlcgen.New(s.pool).MarkMediaAssetCopyCorrupt(ctx, sqlcgen.MarkMediaAssetCopyCorruptParams{
		CopyID: copyID, MediaAssetID: mediaAssetID, Store: storeName,
	})
	if err != nil {
		return false, fmt.Errorf("marking corrupt media asset %d copy in %q: %w", mediaAssetID, storeName, err)
	}
	return updated == 1, nil
}

// DiscardPendingMediaAssetCopy は pending な転送先コピーを無効にする。
func (s *Media) DiscardPendingMediaAssetCopy(ctx context.Context, copyID int64) error {
	if err := sqlcgen.New(s.pool).DiscardPendingMediaAssetCopy(ctx, copyID); err != nil {
		return fmt.Errorf("discarding pending media asset copy %d: %w", copyID, err)
	}
	return nil
}

// ListObsoletePendingMediaAssetTransferCopies は、現在の policy から外れた pending 転送コピーを列挙する。
func (s *Media) ListObsoletePendingMediaAssetTransferCopies(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, limit int32) ([]store.MediaAssetCopyDeleteCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListObsoletePendingMediaAssetTransferCopies(ctx, sqlcgen.ListObsoletePendingMediaAssetTransferCopiesParams{
		StoreName: s.storeName, OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing obsolete pending transfer copies: %w", err)
	}
	out := make([]store.MediaAssetCopyDeleteCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaAssetCopyDeleteCandidate{
			CopyID: row.CopyID, MediaAssetID: row.MediaAssetID, Store: row.Store, RelPath: row.RelPath,
			State: row.State, SizeBytes: row.SizeBytes, Kind: row.Kind, Generation: row.Generation,
		})
	}
	return out, nil
}

// ReclaimObsoletePendingMediaAssetTransferCopy は path lock の下で不要な pending copy を物理削除して退役させる。
func (s *Media) ReclaimObsoletePendingMediaAssetTransferCopy(
	ctx context.Context,
	candidate store.MediaAssetCopyDeleteCandidate,
	originalRules []byte,
	fsStores []string,
	derivedStore string,
	deleteFile func() error,
) (bool, error) {
	if deleteFile == nil {
		return false, errors.New("reclaiming obsolete pending transfer copy: deleter is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning obsolete pending transfer copy reclaim: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	_, err = q.LockPendingMediaAssetTransferCopy(ctx, sqlcgen.LockPendingMediaAssetTransferCopyParams{
		CopyID: candidate.CopyID, StoreName: s.storeName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("locking pending transfer copy %d: %w", candidate.CopyID, mapDatabaseError(err))
	}
	obsolete, err := q.IsPendingMediaAssetTransferCopyObsolete(ctx, sqlcgen.IsPendingMediaAssetTransferCopyObsoleteParams{
		CopyID: candidate.CopyID, OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore,
	})
	if err != nil {
		return false, fmt.Errorf("rechecking pending transfer copy %d policy: %w", candidate.CopyID, mapDatabaseError(err))
	}
	if !obsolete {
		return false, nil
	}
	if err := deleteFile(); err != nil {
		return false, fmt.Errorf("deleting pending transfer copy %d files: %w", candidate.CopyID, err)
	}
	updated, err := q.MarkObsoletePendingMediaAssetTransferCopyDeleted(ctx, sqlcgen.MarkObsoletePendingMediaAssetTransferCopyDeletedParams{
		CopyID: candidate.CopyID, StoreName: s.storeName,
		OriginalRules: originalRules, FsStores: fsStores, DerivedStore: derivedStore,
	})
	if err != nil {
		return false, fmt.Errorf("marking obsolete pending transfer copy %d deleted: %w", candidate.CopyID, mapDatabaseError(err))
	}
	if updated != 1 {
		return false, fmt.Errorf("marking obsolete pending transfer copy %d deleted updated %d rows, want one", candidate.CopyID, updated)
	}
	if err := q.DeleteMediaAssetTransferAttemptsForCopy(ctx, candidate.CopyID); err != nil {
		return false, fmt.Errorf("clearing obsolete transfer attempts for copy %d: %w", candidate.CopyID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing obsolete pending transfer copy reclaim: %w", mapDatabaseError(err))
	}
	return true, nil
}

// ListMediaAssetCopiesToDelete は overlap を過ぎ、現行 policy で削除可能なコピーを列挙する。
func (s *Media) ListMediaAssetCopiesToDelete(ctx context.Context, originalRules []byte, derivedStore string, overlapMicros int64, limit int32) ([]store.MediaAssetCopyDeleteCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListMediaAssetCopiesToDelete(ctx, sqlcgen.ListMediaAssetCopiesToDeleteParams{
		StoreName: s.storeName, OriginalRules: originalRules, DerivedStore: derivedStore, Overlap: overlapMicros, RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing stale media asset copies: %w", err)
	}
	out := make([]store.MediaAssetCopyDeleteCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MediaAssetCopyDeleteCandidate{
			CopyID: row.CopyID, MediaAssetID: row.MediaAssetID, Store: row.Store,
			RelPath: row.RelPath, State: row.State, SizeBytes: row.SizeBytes, Kind: row.Kind, Generation: row.Generation,
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
		RelPath: row.RelPath, State: row.State, SizeBytes: row.SizeBytes, Kind: row.Kind, Generation: row.Generation,
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
		OriginalRules: originalRules, DerivedStore: derivedStore, Overlap: overlapMicros,
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
	current, ok, err := s.GetMediaAssetCopyForDeletion(ctx, candidate)
	if err != nil || !ok || current.State != "deleting" {
		return false, err
	}
	if err := s.MarkDeleted(ctx, candidate.CopyID); err != nil {
		return false, err
	}
	return true, nil
}
