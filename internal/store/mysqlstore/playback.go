package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

// Playback は視聴状態の永続化を MySQL で実装する。
type Playback struct {
	db *sql.DB
}

var _ store.PlaybackState = (*Playback)(nil)

// NewPlayback は MySQL を使う Playback を作る。
func NewPlayback(db *sql.DB) *Playback {
	return &Playback{db: db}
}

// lockPlayableRecording は録画の行を排他で取り、存在しない・完全削除済みなら store.ErrNotFound を返す。
// INSERT … VALUES の upsert は録画の状態を条件にできないので、存在と完全削除は先に録画の行を読んで判定する。
// FOR UPDATE で取り、判定から書き込みまでの間に完全削除が挟まらないようにする。
func lockPlayableRecording(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	var purgedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT purged_at FROM recordings WHERE id = ? FOR UPDATE`, recordingID).Scan(&purgedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && purgedAt.Valid) {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("locking recording %d for playback state: %w", recordingID, err)
	}
	return nil
}

// SavePosition は録画の再生位置を保存する。
func (s *Playback) SavePosition(ctx context.Context, recordingID, positionMs int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockPlayableRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO recording_playback_positions (recording_id, position_ms, updated_at)
VALUES (?, ?, CURRENT_TIMESTAMP(6)) AS incoming
ON DUPLICATE KEY UPDATE position_ms = incoming.position_ms, updated_at = incoming.updated_at`, recordingID, positionMs)
		if err != nil {
			return fmt.Errorf("saving playback position for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// ClearPosition は録画の再生位置を冪等に削除する。
func (s *Playback) ClearPosition(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockPlayableRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_playback_positions WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("deleting playback position for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// MarkWatched は視聴済み印を付け、同じ録画の再生位置を同じ tx で消す。
// 再生位置の削除が失敗すると、印の書き込みも巻き戻す。
func (s *Playback) MarkWatched(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockPlayableRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_watched (recording_id, watched_at)
VALUES (?, CURRENT_TIMESTAMP(6)) AS incoming
ON DUPLICATE KEY UPDATE watched_at = incoming.watched_at`, recordingID); err != nil {
			return fmt.Errorf("saving watched marker for recording %d: %w", recordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_playback_positions WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("clearing playback position for watched recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// ClearWatchedForEvent は録画と同じ放送イベント（network_id・service_id・program_start_at が一致）の
// 視聴済み印をすべて消す。ごみ箱・置き換え済みの行も対象に含める。
func (s *Playback) ClearWatchedForEvent(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockPlayableRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE w FROM recording_watched w
JOIN recordings sibling ON sibling.id = w.recording_id
JOIN recordings selected ON selected.id = ?
WHERE sibling.network_id = selected.network_id
  AND sibling.service_id = selected.service_id
  AND sibling.program_start_at = selected.program_start_at`, recordingID); err != nil {
			return fmt.Errorf("clearing watched markers for recording event %d: %w", recordingID, err)
		}
		return nil
	})
}
