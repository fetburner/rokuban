package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

var _ store.EncodeAttempts = (*Media)(nil)

// Begin は試行を開始し、上限に達した場合は terminal を返す。
func (s *Media) Begin(ctx context.Context, recordingID int64, profile string, limit int32) (store.EncodeAttemptStart, error) {
	for race := 0; race < 3; race++ {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return store.EncodeAttemptStart{}, fmt.Errorf("beginning encode attempt: %w", err)
		}
		q := sqlcgen.New(tx)
		inserted, err := q.CreateRecordingEncodeAttemptRunning(ctx, sqlcgen.CreateRecordingEncodeAttemptRunningParams{
			RecordingID: recordingID,
			Profile:     profile,
		})
		if err != nil {
			rollback(tx)
			return store.EncodeAttemptStart{}, fmt.Errorf("creating encode attempt: %w", err)
		}
		if inserted == 1 {
			if err := tx.Commit(ctx); err != nil {
				return store.EncodeAttemptStart{}, fmt.Errorf("committing encode attempt: %w", err)
			}
			return store.EncodeAttemptStart{Count: 1}, nil
		}

		prior, err := q.GetRecordingEncodeAttemptForUpdate(ctx, sqlcgen.GetRecordingEncodeAttemptForUpdateParams{
			RecordingID: recordingID,
			Profile:     profile,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			rollback(tx)
			continue
		}
		if err != nil {
			rollback(tx)
			return store.EncodeAttemptStart{}, fmt.Errorf("loading prior encode attempt: %w", err)
		}

		priorCount := prior.AttemptCount
		if priorCount == 0 {
			// A pre-migration row still proves that an attempt started.
			priorCount = 1
		}
		start := store.EncodeAttemptStart{}
		if prior.State == "running" {
			start.DeadAttempt = priorCount
			start.RestoreState = "failed"
			deadReason := "encode process died before completing"
			start.RestoreError = &deadReason
			start.RestoreAttemptCount = priorCount
			attemptedAt, err := q.UpdateRecordingEncodeAttemptFailed(ctx, sqlcgen.UpdateRecordingEncodeAttemptFailedParams{
				RecordingID:  recordingID,
				Profile:      profile,
				Error:        &deadReason,
				AttemptCount: prior.AttemptCount,
			})
			if err != nil {
				rollback(tx)
				return store.EncodeAttemptStart{}, fmt.Errorf("marking dead encode attempt failed: %w", err)
			}
			start.RestoreAttemptedAt = attemptedAt
		} else {
			start.RestoreState = prior.State
			start.RestoreError = prior.Error
			start.RestoreAttemptedAt = prior.AttemptedAt
			start.RestoreAttemptCount = priorCount
		}

		if priorCount >= limit {
			start.Count = priorCount
			start.Terminal = true
			if err := tx.Commit(ctx); err != nil {
				return store.EncodeAttemptStart{}, fmt.Errorf("committing terminal encode attempt: %w", err)
			}
			return start, nil
		}

		start.Count = priorCount + 1
		rows, err := q.UpdateRecordingEncodeAttemptRunning(ctx, sqlcgen.UpdateRecordingEncodeAttemptRunningParams{
			RecordingID: recordingID,
			Profile:     profile,
			NextCount:   start.Count,
		})
		if err != nil || rows != 1 {
			rollback(tx)
			if err != nil {
				return store.EncodeAttemptStart{}, fmt.Errorf("starting encode attempt: %w", err)
			}
			return store.EncodeAttemptStart{}, fmt.Errorf("starting encode attempt: updated %d rows", rows)
		}
		if err := tx.Commit(ctx); err != nil {
			return store.EncodeAttemptStart{}, fmt.Errorf("committing encode attempt: %w", err)
		}
		return start, nil
	}
	return store.EncodeAttemptStart{}, fmt.Errorf("starting encode attempt: row changed repeatedly")
}

// Fail は現在の fencing token が一致する実行中試行を失敗にし、更新できたかを返す。
func (s *Media) Fail(ctx context.Context, recordingID int64, profile string, attempt int32, message string) (bool, error) {
	_, err := sqlcgen.New(s.pool).UpdateRecordingEncodeAttemptFailed(ctx, sqlcgen.UpdateRecordingEncodeAttemptFailedParams{
		RecordingID:  recordingID,
		Profile:      profile,
		Error:        &message,
		AttemptCount: attempt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("updating failed encode attempt: %w", err)
	}
	return true, nil
}

// Restore は現在の fencing token が一致するときだけ、開始前の状態へ戻す。
func (s *Media) Restore(ctx context.Context, recordingID int64, profile string, start store.EncodeAttemptStart) error {
	q := sqlcgen.New(s.pool)
	if start.RestoreState == "" {
		if _, err := q.DeleteRecordingEncodeAttemptForAttempt(ctx, sqlcgen.DeleteRecordingEncodeAttemptForAttemptParams{
			RecordingID:  recordingID,
			Profile:      profile,
			AttemptCount: start.Count,
		}); err != nil {
			return fmt.Errorf("removing canceled encode attempt: %w", err)
		}
		return nil
	}
	if _, err := q.RestoreRecordingEncodeAttempt(ctx, sqlcgen.RestoreRecordingEncodeAttemptParams{
		RecordingID:  recordingID,
		Profile:      profile,
		State:        start.RestoreState,
		AttemptedAt:  start.RestoreAttemptedAt,
		RestoreCount: start.RestoreAttemptCount,
		CurrentCount: start.Count,
		Error:        start.RestoreError,
	}); err != nil {
		return fmt.Errorf("restoring prior encode attempt: %w", err)
	}
	return nil
}

// Clear は現在の fencing token が一致する実行中試行を消す。
func (s *Media) Clear(ctx context.Context, recordingID int64, profile string, attempt int32) error {
	if _, err := sqlcgen.New(s.pool).DeleteRecordingEncodeAttemptForAttempt(ctx, sqlcgen.DeleteRecordingEncodeAttemptForAttemptParams{
		RecordingID:  recordingID,
		Profile:      profile,
		AttemptCount: attempt,
	}); err != nil {
		return fmt.Errorf("deleting encode attempt: %w", err)
	}
	return nil
}
