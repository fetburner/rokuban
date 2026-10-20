package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/webhook"
)

const (
	encodeAttemptLimit       int32 = jobs.EncodeAttemptLimit
	encodeProcessDeathReason       = "encode process died before completing"
)

// errEncodeAttemptLimit は jobq.ErrDiscard に包んで返す、ドメイン上限到達の理由。
var errEncodeAttemptLimit = fmt.Errorf("encode attempt limit (%d) reached; fix the cause and re-request with POST /api/recordings/{id}/encode-profiles or cut /reencode", encodeAttemptLimit)

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
	start, err := w.Attempts.Begin(ctx, recordingID, profile, encodeAttemptLimit)
	if err != nil {
		return encodeAttemptStart{}, err
	}
	return encodeAttemptStart{
		count:               start.Count,
		deadAttempt:         start.DeadAttempt,
		terminal:            start.Terminal,
		restoreState:        start.RestoreState,
		restoreError:        start.RestoreError,
		restoreAttemptedAt:  start.RestoreAttemptedAt,
		restoreAttemptCount: start.RestoreAttemptCount,
	}, nil
}

func (w *EncodeWorker) markEncodeAttemptFailed(ctx context.Context, recordingID int64, profile string, attempt int32, encodeErr error) (bool, error) {
	msg := truncateEncodeAttemptError(encodeErr.Error())
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	updated, err := w.Attempts.Fail(writeCtx, recordingID, profile, attempt, msg)
	if err != nil {
		return false, fmt.Errorf("marking encode attempt failed: %w", err)
	}
	return updated, nil
}

func (w *EncodeWorker) restoreEncodeAttempt(ctx context.Context, recordingID int64, profile string, start encodeAttemptStart) error {
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	if err := w.Attempts.Restore(writeCtx, recordingID, profile, store.EncodeAttemptStart{
		Count:               start.count,
		RestoreState:        start.restoreState,
		RestoreError:        start.restoreError,
		RestoreAttemptedAt:  start.restoreAttemptedAt,
		RestoreAttemptCount: start.restoreAttemptCount,
	}); err != nil {
		return fmt.Errorf("restoring encode attempt: %w", err)
	}
	return nil
}

func (w *EncodeWorker) clearEncodeAttempt(ctx context.Context, recordingID int64, profile string, attempt int32) error {
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	if err := w.Attempts.Clear(writeCtx, recordingID, profile, attempt); err != nil {
		return fmt.Errorf("clearing encode attempt: %w", err)
	}
	return nil
}

func shouldCountEncodeFailure(err, ctxErr error) bool {
	_, snoozed := jobq.SnoozeDelay(err)
	return err != nil && !errors.Is(ctxErr, context.Canceled) && !snoozed
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
