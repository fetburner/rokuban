package jobs

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobq"
)

// EnqueueContinuation enqueues a reconcile continuation and logs the enqueue result.
// TestEnqueueContinuation_LogsFailureAndSuccess covers both log paths. It returns no error,
// so an enqueue failure does not fail the current reconcile pass.
func EnqueueContinuation(
	ctx context.Context,
	enqueuer jobq.Enqueuer,
	args jobq.Args,
	logPrefix string,
	successMessage string,
	errorAttrs []any,
	successAttrs []any,
) {
	var err error
	if enqueuer == nil {
		err = errors.New("job enqueuer is nil")
	} else {
		_, err = enqueuer.Enqueue(ctx, args)
	}
	if err != nil {
		attrs := make([]any, 0, len(errorAttrs)+2)
		attrs = append(attrs, errorAttrs...)
		attrs = append(attrs, "err", err)
		slog.Error(logPrefix+": failed to enqueue continuation", attrs...)
		return
	}
	slog.Warn(logPrefix+": "+successMessage, successAttrs...)
}
