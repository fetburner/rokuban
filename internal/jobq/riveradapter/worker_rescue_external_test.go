package riveradapter_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

type encodeRescueExecution struct {
	ctx         context.Context
	attempt     int
	remaining   time.Duration
	previousErr error
}

type encodeRescueTestWorker struct {
	river.WorkerDefaults[jobs.EncodeJobArgs]
	timeout    time.Duration
	executions chan encodeRescueExecution
	release    <-chan struct{}
	previous   context.Context
	mu         sync.Mutex
}

func (w *encodeRescueTestWorker) Timeout(*river.Job[jobs.EncodeJobArgs]) time.Duration {
	return w.timeout
}

func (w *encodeRescueTestWorker) Work(ctx context.Context, job *river.Job[jobs.EncodeJobArgs]) error {
	deadline, ok := ctx.Deadline()
	remaining := time.Duration(0)
	if ok {
		remaining = time.Until(deadline)
	}
	w.mu.Lock()
	var previousErr error
	if w.previous != nil {
		previousErr = w.previous.Err()
	}
	w.previous = ctx
	w.mu.Unlock()
	w.executions <- encodeRescueExecution{ctx: ctx, attempt: job.Attempt, remaining: remaining, previousErr: previousErr}
	<-w.release // Deliberately ignore ctx cancellation until the test releases this attempt.
	return nil
}

// TestEncodeWorker_RiverRescuesAfterWorkerTimeout checks that Work's deadline comes
// from the per-worker Timeout and that a retry starts after the previous deadline,
// using a live PostgreSQL client.
func TestEncodeWorker_RiverRescuesAfterWorkerTimeout(t *testing.T) {
	pool := testutil.SetupDB(t)
	const workerTimeout = 5 * time.Second
	executions := make(chan encodeRescueExecution, 2)
	release := make(chan struct{})
	worker := &encodeRescueTestWorker{
		timeout:    workerTimeout,
		executions: executions,
		release:    release,
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:               map[string]river.QueueConfig{jobs.EncodeQueue: {MaxWorkers: 2}},
		Workers:              workers,
		JobTimeout:           100 * time.Millisecond,
		RescueStuckJobsAfter: 200 * time.Millisecond,
		TestOnly:             true,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}

	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	started := false
	t.Cleanup(func() {
		releaseAll()
		if started {
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := client.Stop(stopCtx); err != nil {
				t.Errorf("stopping River client: %v", err)
			}
		}
	})

	if _, err := client.Insert(context.Background(), jobs.EncodeJobArgs{
		RecordingID: 1,
		Profile:     "rescue-test",
	}, &river.InsertOpts{Queue: jobs.EncodeQueue, MaxAttempts: 25}); err != nil {
		t.Fatalf("inserting encode job: %v", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("starting River client: %v", err)
	}
	started = true

	var first encodeRescueExecution
	select {
	case first = <-executions:
	case <-time.After(10 * time.Second):
		t.Fatal("first encode execution did not start")
	}
	if first.attempt != 1 {
		t.Fatalf("first River attempt = %d, want 1", first.attempt)
	}

	if first.remaining <= time.Second || first.remaining > workerTimeout {
		t.Fatalf("Work deadline remaining = %s, want (1s, %s]", first.remaining, workerTimeout)
	}

	select {
	case <-first.ctx.Done():
		if !errors.Is(first.ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("first Work context error = %v, want DeadlineExceeded", first.ctx.Err())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("River did not cancel first Work after its worker timeout %s", workerTimeout)
	}

	// The public client config does not expose River's 30-second rescuer cadence.
	// Allow two intervals for leadership startup and the next rescue tick.
	select {
	case next := <-executions:
		// 未検証: 早すぎる救出をどこまで検出できるかは、非公開の rescuer 周期に依存する。
		if !errors.Is(next.previousErr, context.DeadlineExceeded) {
			t.Fatalf("retry before previous timeout: %v", next.previousErr)
		}
		if next.attempt < 2 {
			t.Fatalf("rescued River attempt = %d, want at least 2", next.attempt)
		}
	case <-time.After(75 * time.Second):
		t.Fatal("River JobRescuer did not retry the timed-out encode job")
	}

	// The first Work deliberately ignored cancellation. Release both copies before
	// stopping the client so the test remains self-contained.
	releaseAll()
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("stopping River client: %v", err)
	}
	started = false
}
