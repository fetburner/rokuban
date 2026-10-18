package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Recordings は録画のごみ箱と完全削除の永続化を PostgreSQL で実装する。
type Recordings struct {
	pool *pgxpool.Pool
}

// NewRecordings は PostgreSQL を使う Recordings を作る。
func NewRecordings(pool *pgxpool.Pool) *Recordings {
	return &Recordings{pool: pool}
}

// SoftDelete は録画を論理削除する。既にごみ箱にある場合も成功する。
func (s *Recordings) SoftDelete(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning soft delete for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)

	if _, err := sqlcgen.New(tx).SoftDeleteRecording(ctx, recordingID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("soft-deleting recording %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing soft delete for recording %d: %w", recordingID, err)
	}
	return nil
}

// Restore は録画を復元し、同じ録画の即時削除要求を取り下げる。
func (s *Recordings) Restore(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning restore for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if _, err := q.RestoreRecording(ctx, recordingID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("restoring recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if err := q.WithdrawRecordingPurgeRequest(ctx, recordingID); err != nil {
		return fmt.Errorf("withdrawing purge request for recording %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing restore for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return nil
}

// RequestPurge は即時削除を要求し、まだごみ箱に無ければ論理削除も行う。
func (s *Recordings) RequestPurge(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning purge request for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)

	if _, err := sqlcgen.New(tx).MarkRecordingPurgeRequested(ctx, recordingID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("marking recording %d for purge: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing purge request for recording %d: %w", recordingID, err)
	}
	return nil
}

// MarkPurged は削除対象が残っていない録画を完全削除済みにし、今回確定した録画を返す。
func (s *Recordings) MarkPurged(ctx context.Context, graceCutoff time.Time) ([]store.PurgedRecording, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning purged recording mark: %w", err)
	}
	defer rollback(tx)

	rows, err := sqlcgen.New(tx).MarkPurgedRecordings(ctx, graceCutoff)
	if err != nil {
		return nil, fmt.Errorf("marking purged recordings: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing purged recording mark: %w", err)
	}

	purged := make([]store.PurgedRecording, 0, len(rows))
	for _, row := range rows {
		purged = append(purged, store.PurgedRecording{ID: row.ID, Site: row.Site, Title: row.Title})
	}
	return purged, nil
}

var (
	_ store.RecordingTrash = (*Recordings)(nil)
	_ store.RecordingPurge = (*Recordings)(nil)
)
