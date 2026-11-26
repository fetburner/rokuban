package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.EncodeAttempts = (*Media)(nil)

// Begin は試行行を作るか、既存の試行を直列化して上限と次の token を決める。
func (s *Media) Begin(ctx context.Context, recordingID int64, profile string, limit int32) (store.EncodeAttemptStart, error) {
	var start store.EncodeAttemptStart
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		// INSERT なら 1、重複なら既存の count を保って 0 を接続に記録する。
		// RunEncodePolicy の「attempt begin enforces the supplied limit」と
		// 「stale attempt tokens cannot mutate a newer running attempt」で検査する。
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempted_at, attempt_count)
VALUES (?, ?, 'running', ?, LAST_INSERT_ID(1))
ON DUPLICATE KEY UPDATE attempt_count = attempt_count + LAST_INSERT_ID(0)`, recordingID, profile, now); err != nil {
			return fmt.Errorf("creating encode attempt: %w", err)
		}
		var inserted bool
		if err := tx.QueryRowContext(ctx, "SELECT LAST_INSERT_ID()").Scan(&inserted); err != nil {
			return fmt.Errorf("checking whether encode attempt was inserted: %w", err)
		}
		if inserted {
			start.Count = 1
			return nil
		}

		var priorCount int32
		if err := tx.QueryRowContext(ctx, `
SELECT state, error, attempted_at, attempt_count FROM recording_encode_attempts
WHERE recording_id = ? AND profile = ? FOR UPDATE`, recordingID, profile).
			Scan(&start.RestoreState, &start.RestoreError, &start.RestoreAttemptedAt, &priorCount); err != nil {
			return fmt.Errorf("loading prior encode attempt: %w", err)
		}
		priorCount = max(priorCount, 1)
		start.RestoreAttemptCount = priorCount
		if start.RestoreState == "running" {
			start.DeadAttempt = priorCount
			start.RestoreState = "failed"
			deadReason := "encode process died before completing"
			start.RestoreError = &deadReason
			start.RestoreAttemptedAt = now
		}

		if priorCount >= limit {
			start.Count = priorCount
			start.Terminal = true
			if start.DeadAttempt == 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE recording_encode_attempts SET state = 'failed', error = ?, attempted_at = ?
WHERE recording_id = ? AND profile = ?`, start.RestoreError, now, recordingID, profile); err != nil {
				return fmt.Errorf("marking dead encode attempt failed: %w", err)
			}
			return nil
		}

		start.Count = priorCount + 1
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_encode_attempts SET state = 'running', error = NULL, attempted_at = ?, attempt_count = ?
WHERE recording_id = ? AND profile = ?`, now, start.Count, recordingID, profile); err != nil {
			return fmt.Errorf("starting encode attempt: %w", err)
		}
		return nil
	})
	if err != nil {
		return store.EncodeAttemptStart{}, err
	}
	return start, nil
}

// Fail は fencing token が一致する実行中の試行を失敗にし、更新できたかを返す。
func (s *Media) Fail(ctx context.Context, recordingID int64, profile string, attempt int32, message string) (bool, error) {
	var updated bool
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE recording_encode_attempts SET state = 'failed', error = ?, attempted_at = CURRENT_TIMESTAMP(6)
WHERE recording_id = ? AND profile = ? AND state = 'running' AND attempt_count = ?`,
			message, recordingID, profile, attempt)
		if err != nil {
			return fmt.Errorf("marking encode attempt failed: %w", err)
		}
		// 条件に一致した行は running から failed に変わる。
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting failed encode attempts: %w", err)
		}
		updated = rows == 1
		return nil
	})
	return err == nil && updated, err
}

// Restore は fencing token が一致する試行を開始前の状態へ戻す。
func (s *Media) Restore(ctx context.Context, recordingID int64, profile string, start store.EncodeAttemptStart) error {
	if start.RestoreState == "" {
		return s.Clear(ctx, recordingID, profile, start.Count)
	}
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_encode_attempts SET state = ?, error = ?, attempted_at = ?, attempt_count = ?
WHERE recording_id = ? AND profile = ? AND state = 'running' AND attempt_count = ?`,
			start.RestoreState, start.RestoreError, start.RestoreAttemptedAt, start.RestoreAttemptCount,
			recordingID, profile, start.Count); err != nil {
			return fmt.Errorf("restoring prior encode attempt: %w", err)
		}
		return nil
	})
}

// Clear は fencing token が一致する実行中の試行を消す。
func (s *Media) Clear(ctx context.Context, recordingID int64, profile string, attempt int32) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM recording_encode_attempts
WHERE recording_id = ? AND profile = ? AND state = 'running' AND attempt_count = ?`,
			recordingID, profile, attempt); err != nil {
			return fmt.Errorf("deleting encode attempt: %w", err)
		}
		return nil
	})
}
