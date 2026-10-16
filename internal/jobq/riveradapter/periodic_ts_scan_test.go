package riveradapter

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/tsscan"
)

type blockingTSScanReconcileWorker struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (worker blockingTSScanReconcileWorker) Work(ctx context.Context, _ tsscan.ReconcileArgs) error {
	worker.started <- struct{}{}
	select {
	case <-worker.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestTSScanReconcilePeriodicUsesScanQueueAndCoalesces(t *testing.T) {
	pool := testutil.SetupDB(t)
	registry := jobq.NewRegistry()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	Register(registry, blockingTSScanReconcileWorker{started: started, release: release})

	adapter, err := NewAdapter(pool, registry, testQueueFor, Config{
		Spec: jobq.WorkerSpec{
			Queues:       []jobq.QueueSpec{plainQueue(jobs.TSScanQueue, 1)},
			PeriodicJobs: []jobq.PeriodicJobSpec{{Interval: oneHour, Args: tsscan.ReconcileArgs{}}},
		},
		Queues:               []string{jobs.TSScanQueue},
		SchedulePeriodicJobs: true,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	startedAdapter := false
	t.Cleanup(func() {
		close(release)
		if startedAdapter {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := adapter.Stop(ctx); err != nil {
				t.Errorf("Adapter.Stop: %v", err)
			}
		}
	})

	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("Adapter.Start: %v", err)
	}
	startedAdapter = true
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the run-on-start TS scan reconciliation job")
	}

	rows := riveradaptertest.MustListRiverJobsOfKind(t, context.Background(), pool, "ts_scan_reconcile")
	if len(rows) != 1 {
		t.Fatalf("ts_scan_reconcile row count = %d, want 1", len(rows))
	}
	if rows[0].Queue != "ts_scan" {
		t.Errorf("periodic ts_scan_reconcile queue = %q, want literal %q", rows[0].Queue, "ts_scan")
	}

	inserted, err := adapter.Enqueue(context.Background(), tsscan.ReconcileArgs{})
	if err != nil {
		t.Fatalf("enqueueing duplicate reconciliation args: %v", err)
	}
	if !inserted.Coalesced {
		t.Error("adapter enqueue was not coalesced with the periodic reconciliation job")
	}
}
