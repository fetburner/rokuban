package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/webhook"
)

const (
	encodeAttemptLimit       int32 = 25
	encodeProcessDeathReason       = "encode process died before completing"
)

type encodeAttemptStart struct {
	count int32

	// deadAttempt is non-zero when the previous row was still running. The next
	// start counts that attempt as failed before it starts count+1.
	deadAttempt int32
	terminal    bool

	restoreState        string
	restoreError        *string
	restoreAttemptedAt  time.Time
	restoreAttemptCount int32
}

// beginEncodeAttempt creates the next domain attempt and returns its fencing token.
func (w *EncodeWorker) beginEncodeAttempt(ctx context.Context, recordingID int64, profile string) (encodeAttemptStart, error) {
	for race := 0; race < 3; race++ {
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			return encodeAttemptStart{}, fmt.Errorf("beginning encode attempt: %w", err)
		}
		q := sqlcgen.New(tx)
		inserted, err := q.CreateRecordingEncodeAttemptRunning(ctx, sqlcgen.CreateRecordingEncodeAttemptRunningParams{
			RecordingID: recordingID,
			Profile:     profile,
		})
		if err != nil {
			_ = tx.Rollback(context.Background())
			return encodeAttemptStart{}, fmt.Errorf("creating encode attempt: %w", err)
		}
		if inserted == 1 {
			if err := tx.Commit(ctx); err != nil {
				return encodeAttemptStart{}, fmt.Errorf("committing encode attempt: %w", err)
			}
			return encodeAttemptStart{count: 1}, nil
		}

		prior, err := q.GetRecordingEncodeAttemptForUpdate(ctx, sqlcgen.GetRecordingEncodeAttemptForUpdateParams{
			RecordingID: recordingID,
			Profile:     profile,
		})
		if errors.Is(err, pgx5.ErrNoRows) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			return encodeAttemptStart{}, fmt.Errorf("loading prior encode attempt: %w", err)
		}

		priorCount := prior.AttemptCount
		if priorCount == 0 {
			// Rows written by a pre-migration worker have no count. Such a row
			// still proves that at least one attempt started.
			priorCount = 1
		}
		start := encodeAttemptStart{}
		if prior.State == "running" {
			start.deadAttempt = priorCount
			deadReason := encodeProcessDeathReason
			start.restoreState = "failed"
			start.restoreError = &deadReason
			start.restoreAttemptedAt = time.Now().UTC()
			start.restoreAttemptCount = priorCount
		} else {
			start.restoreState = prior.State
			start.restoreError = prior.Error
			start.restoreAttemptedAt = prior.AttemptedAt
			start.restoreAttemptCount = priorCount
		}

		if priorCount >= encodeAttemptLimit {
			start.count = priorCount
			start.terminal = true
			if prior.State == "running" {
				deadReason := encodeProcessDeathReason
				rows, err := q.UpdateRecordingEncodeAttemptFailed(ctx, sqlcgen.UpdateRecordingEncodeAttemptFailedParams{
					RecordingID:  recordingID,
					Profile:      profile,
					Error:        &deadReason,
					AttemptCount: prior.AttemptCount,
				})
				if err != nil || rows != 1 {
					_ = tx.Rollback(context.Background())
					if err != nil {
						return encodeAttemptStart{}, fmt.Errorf("marking dead encode attempt failed: %w", err)
					}
					return encodeAttemptStart{}, fmt.Errorf("marking dead encode attempt failed: updated %d rows", rows)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return encodeAttemptStart{}, fmt.Errorf("committing terminal encode attempt: %w", err)
			}
			return start, nil
		}

		start.count = priorCount + 1
		rows, err := q.UpdateRecordingEncodeAttemptRunning(ctx, sqlcgen.UpdateRecordingEncodeAttemptRunningParams{
			RecordingID: recordingID,
			Profile:     profile,
			NextCount:   start.count,
		})
		if err != nil || rows != 1 {
			_ = tx.Rollback(context.Background())
			if err != nil {
				return encodeAttemptStart{}, fmt.Errorf("starting encode attempt: %w", err)
			}
			return encodeAttemptStart{}, fmt.Errorf("starting encode attempt: updated %d rows", rows)
		}
		if err := tx.Commit(ctx); err != nil {
			return encodeAttemptStart{}, fmt.Errorf("committing encode attempt: %w", err)
		}
		return start, nil
	}
	return encodeAttemptStart{}, fmt.Errorf("starting encode attempt: row changed repeatedly")
}

func (w *EncodeWorker) markEncodeAttemptFailed(ctx context.Context, recordingID int64, profile string, attempt int32, encodeErr error) (bool, error) {
	msg := truncateEncodeAttemptError(encodeErr.Error())
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	rows, err := sqlcgen.New(w.Pool).UpdateRecordingEncodeAttemptFailed(writeCtx, sqlcgen.UpdateRecordingEncodeAttemptFailedParams{
		RecordingID:  recordingID,
		Profile:      profile,
		Error:        &msg,
		AttemptCount: attempt,
	})
	if err != nil {
		return false, fmt.Errorf("marking encode attempt failed: %w", err)
	}
	return rows == 1, nil
}

func (w *EncodeWorker) restoreEncodeAttempt(ctx context.Context, recordingID int64, profile string, start encodeAttemptStart) error {
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	q := sqlcgen.New(w.Pool)
	if start.restoreState == "" {
		if _, err := q.DeleteRecordingEncodeAttemptForAttempt(writeCtx, sqlcgen.DeleteRecordingEncodeAttemptForAttemptParams{
			RecordingID:  recordingID,
			Profile:      profile,
			AttemptCount: start.count,
		}); err != nil {
			return fmt.Errorf("removing canceled encode attempt: %w", err)
		}
		return nil
	}
	if _, err := q.RestoreRecordingEncodeAttempt(writeCtx, sqlcgen.RestoreRecordingEncodeAttemptParams{
		RecordingID:  recordingID,
		Profile:      profile,
		State:        start.restoreState,
		AttemptedAt:  start.restoreAttemptedAt,
		RestoreCount: start.restoreAttemptCount,
		CurrentCount: start.count,
		Error:        start.restoreError,
	}); err != nil {
		return fmt.Errorf("restoring prior encode attempt: %w", err)
	}
	return nil
}

func (w *EncodeWorker) clearEncodeAttempt(ctx context.Context, recordingID int64, profile string, attempt int32) error {
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	if _, err := sqlcgen.New(w.Pool).DeleteRecordingEncodeAttemptForAttempt(writeCtx, sqlcgen.DeleteRecordingEncodeAttemptForAttemptParams{
		RecordingID:  recordingID,
		Profile:      profile,
		AttemptCount: attempt,
	}); err != nil {
		return fmt.Errorf("clearing encode attempt: %w", err)
	}
	return nil
}

func shouldCountEncodeFailure(err, ctxErr error) bool {
	var snooze *rivertype.JobSnoozeError
	return err != nil && !errors.Is(ctxErr, context.Canceled) && !errors.As(err, &snooze)
}

func (w *EncodeWorker) notifyEncodeFailure(ctx context.Context, args jobs.EncodeJobArgs, attempt int32) {
	w.notify(context.WithoutCancel(ctx), webhook.Event{
		Type:        webhook.EventEncodeFailed,
		RecordingID: args.RecordingID,
		Status:      "failed",
		Profile:     args.Profile,
		Attempt:     int(attempt),
		MaxAttempts: int(encodeAttemptLimit),
	})
}

func logEncodeAttemptWriteFailure(operation string, recordingID int64, profile string, err error) {
	slog.Warn("encode: attempt state write failed", "operation", operation,
		"recording_id", recordingID, "profile", profile, "err", err)
}
