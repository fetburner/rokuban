// Package testutil contains helpers for exercising workers through jobqfake.
package testutil

import (
	"context"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/jobqfake"
)

// RunWorkerOnce enqueues args into jobqfake, waits for one delivery, and returns the worker's
// result. The caller's context values and cancellation are preserved while the fake's timeout
// also applies. The observing worker reports nil to jobqfake so the call is never retried.
func RunWorkerOnce[T jobq.Args](ctx context.Context, worker jobq.Worker[T], args T) error {
	if ctx == nil {
		ctx = context.Background()
	}
	observed := &observingWorker[T]{worker: worker, callerCtx: ctx, result: make(chan error, 1)}
	registry := jobq.NewRegistry()
	jobq.Register(registry, observed)
	queue := jobqfake.New(registry, jobqfake.Config{})
	if _, err := queue.Enqueue(context.Background(), args); err != nil {
		return fmt.Errorf("jobqfake: enqueueing %q: %w", args.Kind(), err)
	}
	if err := queue.Start(context.Background()); err != nil {
		return fmt.Errorf("jobqfake: starting runner: %w", err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelWait()
	waitErr := queue.WaitIdle(waitCtx)
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	if err := queue.Stop(stopCtx); err != nil {
		return fmt.Errorf("jobqfake: stopping runner: %w", err)
	}
	if waitErr != nil {
		return fmt.Errorf("jobqfake: waiting for delivery: %w", waitErr)
	}
	return <-observed.result
}

type observingWorker[T jobq.Args] struct {
	worker    jobq.Worker[T]
	callerCtx context.Context
	result    chan error
}

func (worker *observingWorker[T]) Work(queueCtx context.Context, args T) error {
	ctx, cancel := mergeContexts(queueCtx, worker.callerCtx)
	err := worker.worker.Work(ctx, args)
	cancel()
	worker.result <- err
	return nil
}

func mergeContexts(queueCtx, callerCtx context.Context) (context.Context, context.CancelFunc) {
	if callerCtx == nil {
		callerCtx = context.Background()
	}
	queueDeadline, queueHasDeadline := queueCtx.Deadline()
	callerDeadline, callerHasDeadline := callerCtx.Deadline()
	var cancel context.CancelFunc
	var ctx context.Context
	if queueHasDeadline && (!callerHasDeadline || queueDeadline.Before(callerDeadline)) {
		ctx, cancel = context.WithDeadline(callerCtx, queueDeadline)
	} else {
		ctx, cancel = context.WithCancel(callerCtx)
	}
	stop := context.AfterFunc(queueCtx, cancel)
	if queueCtx.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}
