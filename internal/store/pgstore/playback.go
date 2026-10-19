package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Playback は視聴状態の永続化を PostgreSQL で実装する。
type Playback struct {
	pool *pgxpool.Pool
}

// NewPlayback は PostgreSQL を使う Playback を作る。
func NewPlayback(pool *pgxpool.Pool) *Playback {
	return &Playback{pool: pool}
}

// SavePosition は録画の再生位置を保存する。
func (s *Playback) SavePosition(ctx context.Context, recordingID, positionMs int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning playback position save for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)

	rows, err := sqlcgen.New(tx).UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{
		RecordingID: recordingID,
		PositionMs:  positionMs,
	})
	if err != nil {
		return fmt.Errorf("saving playback position for recording %d: %w", recordingID, err)
	}
	if rows == 0 {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing playback position for recording %d: %w", recordingID, err)
	}
	return nil
}

// ClearPosition は録画の再生位置を冪等に削除する。
func (s *Playback) ClearPosition(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning playback position delete for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	exists, err := q.RecordingExistsForPlaybackState(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking recording %d for playback position delete: %w", recordingID, err)
	}
	if !exists {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if _, err := q.DeleteRecordingPlaybackPosition(ctx, recordingID); err != nil {
		return fmt.Errorf("deleting playback position for recording %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing playback position delete for recording %d: %w", recordingID, err)
	}
	return nil
}

// MarkWatched は視聴済み印を付け、同じ録画の再生位置を原子的に消す。
func (s *Playback) MarkWatched(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning watched marker write for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	rows, err := q.UpsertRecordingWatched(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("saving watched marker for recording %d: %w", recordingID, err)
	}
	if rows == 0 {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if _, err := q.DeleteRecordingPlaybackPosition(ctx, recordingID); err != nil {
		return fmt.Errorf("clearing playback position for watched recording %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing watched marker for recording %d: %w", recordingID, err)
	}
	return nil
}

// ClearWatchedForEvent は録画と同じ放送イベントに属する視聴済み印をすべて消す。
func (s *Playback) ClearWatchedForEvent(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning watched marker delete for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	exists, err := q.RecordingExistsForPlaybackState(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking recording %d for watched marker delete: %w", recordingID, err)
	}
	if !exists {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if _, err := q.DeleteRecordingWatchedForEvent(ctx, recordingID); err != nil {
		return fmt.Errorf("clearing watched markers for recording event %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing watched marker delete for recording %d: %w", recordingID, err)
	}
	return nil
}

var _ store.PlaybackState = (*Playback)(nil)
