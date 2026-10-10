package worker

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
)

type encodeRescueExecution struct {
	ctx     context.Context
	attempt int
}

type encodeRescueTestWorker struct {
	river.WorkerDefaults[EncodeJobArgs]
	timeout    time.Duration
	executions chan encodeRescueExecution
	release    <-chan struct{}
}

func (w *encodeRescueTestWorker) Timeout(*river.Job[EncodeJobArgs]) time.Duration {
	return w.timeout
}

func (w *encodeRescueTestWorker) Work(ctx context.Context, job *river.Job[EncodeJobArgs]) error {
	w.executions <- encodeRescueExecution{ctx: ctx, attempt: job.Attempt}
	<-w.release // Deliberately ignore ctx cancellation until the test releases this attempt.
	return nil
}

// TestEncodeWorker_RiverRescuesAfterWorkerTimeout verifies River's public per-worker
// Timeout contract with a live PostgreSQL client. RescueStuckJobsAfter is shorter than
// the worker timeout, so an early second execution means River ignored the override.
func TestEncodeWorker_RiverRescuesAfterWorkerTimeout(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	const workerTimeout = 1500 * time.Millisecond
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

	if _, err := client.Insert(context.Background(), EncodeJobArgs{RecordingID: 1, Profile: "rescue-test"}, nil); err != nil {
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

	// RescueStuckJobsAfter elapsed, but the per-worker Timeout has not. The running
	// Work intentionally ignores cancellation so only the rescuer can start attempt 2.
	select {
	case next := <-executions:
		t.Fatalf("River started attempt %d before worker timeout %s", next.attempt, workerTimeout)
	case <-time.After(500 * time.Millisecond):
	}
	if err := first.ctx.Err(); err != nil {
		t.Fatalf("first Work context ended before its worker timeout: %v", err)
	}

	select {
	case <-first.ctx.Done():
		if !errors.Is(first.ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("first Work context error = %v, want DeadlineExceeded", first.ctx.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("River did not cancel first Work after its worker timeout %s", workerTimeout)
	}

	// River's public maintenance cadence is not configurable here. Give its real
	// JobRescuer time to observe the expired worker-specific deadline. River's public
	// client config does not expose its 30-second maintenance interval, so allow two
	// intervals for leadership startup and the next rescuer tick.
	select {
	case next := <-executions:
		if next.attempt < 2 {
			t.Fatalf("rescued River attempt = %d, want at least 2", next.attempt)
		}
	case <-time.After(75 * time.Second):
		t.Fatal("River JobRescuer did not retry the timed-out encode job")
	}

	// The first Work has deliberately ignored cancellation. Let both copies return
	// before stopping the client; this also keeps the integration test self-contained.
	releaseAll()
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("stopping River client: %v", err)
	}
	started = false
}
