package riveradapter_test

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

type periodicSiteDelivery struct {
	kind string
	site string
}

type epgSyncSiteRecorder struct{ deliveries chan<- periodicSiteDelivery }

func (worker epgSyncSiteRecorder) Work(_ context.Context, args jobs.EpgSyncArgs) error {
	worker.deliveries <- periodicSiteDelivery{kind: args.Kind(), site: args.Site}
	return nil
}

type tunerSyncSiteRecorder struct{ deliveries chan<- periodicSiteDelivery }

func (worker tunerSyncSiteRecorder) Work(_ context.Context, args jobs.TunerSyncArgs) error {
	worker.deliveries <- periodicSiteDelivery{kind: args.Kind(), site: args.Site}
	return nil
}

type rulerPassSiteRecorder struct{ deliveries chan<- periodicSiteDelivery }

func (worker rulerPassSiteRecorder) Work(_ context.Context, args jobs.RulerPassArgs) error {
	worker.deliveries <- periodicSiteDelivery{kind: args.Kind(), site: args.Site}
	return nil
}

type reconcilePassSiteRecorder struct{ deliveries chan<- periodicSiteDelivery }

func (worker reconcilePassSiteRecorder) Work(_ context.Context, args jobs.ReconcilePassArgs) error {
	worker.deliveries <- periodicSiteDelivery{kind: args.Kind(), site: args.Site}
	return nil
}

type recordSweepSiteRecorder struct{ deliveries chan<- periodicSiteDelivery }

func (worker recordSweepSiteRecorder) Work(_ context.Context, args jobs.RecordSweepArgs) error {
	worker.deliveries <- periodicSiteDelivery{kind: args.Kind(), site: args.Site}
	return nil
}

// TestPeriodicSiteJobsRunOnStartWithBoundArgs verifies that each registered site job is
// inserted, decoded, and delivered with its own Args.Site on startup.
func TestPeriodicSiteJobsRunOnStartWithBoundArgs(t *testing.T) {
	pool := testutil.SetupDB(t)
	sites := []string{"tokyo", "takamatsu"}
	kinds := []string{"epg_sync", "tuner_sync", "ruler_pass", "reconcile_pass", "record_sweep"}
	want := make(map[periodicSiteDelivery]struct{}, len(sites)*len(kinds))
	for _, site := range sites {
		for _, kind := range kinds {
			want[periodicSiteDelivery{kind: kind, site: site}] = struct{}{}
		}
	}
	deliveries := make(chan periodicSiteDelivery, len(want))
	registry := jobq.NewRegistry()
	riveradapter.Register(registry, epgSyncSiteRecorder{deliveries})
	riveradapter.Register(registry, tunerSyncSiteRecorder{deliveries})
	riveradapter.Register(registry, rulerPassSiteRecorder{deliveries})
	riveradapter.Register(registry, reconcilePassSiteRecorder{deliveries})
	riveradapter.Register(registry, recordSweepSiteRecorder{deliveries})

	spec := jobq.WorkerSpec{Queues: []jobq.QueueSpec{{Logical: jobs.RulerQueue, Physical: []string{jobs.RulerQueue}, MaxWorkers: 1}}}
	for _, logical := range []string{jobs.EpgQueue, jobs.ReconcilerQueue, jobs.RecordSweepQueue} {
		queue := jobq.QueueSpec{Logical: logical, MaxWorkers: 1}
		for _, site := range sites {
			queue.Physical = append(queue.Physical, jobs.PhysicalQueueName(logical, site))
		}
		spec.Queues = append(spec.Queues, queue)
	}
	for _, site := range sites {
		spec.PeriodicJobs = append(spec.PeriodicJobs,
			jobq.PeriodicJobSpec{Interval: time.Hour, Args: jobs.EpgSyncArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: time.Hour, Args: jobs.TunerSyncArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: time.Hour, Args: jobs.RulerPassArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: time.Hour, Args: jobs.ReconcilePassArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: time.Hour, Args: jobs.RecordSweepArgs{Site: site}},
		)
	}
	adapter, err := riveradapter.NewAdapter(pool, registry, jobs.PhysicalQueueFor, riveradapter.Config{
		Spec:                 spec,
		SchedulePeriodicJobs: true,
		TestOnly:             true,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	started := false
	t.Cleanup(func() {
		if !started {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := adapter.Stop(stopCtx); err != nil {
			t.Errorf("Adapter.Stop: %v", err)
		}
	})
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("Adapter.Start: %v", err)
	}
	started = true

	seen := make(map[periodicSiteDelivery]bool, len(want))
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for len(seen) < len(want) {
		select {
		case got := <-deliveries:
			if _, ok := want[got]; !ok {
				t.Errorf("unexpected periodic delivery = %+v", got)
				continue
			}
			if seen[got] {
				t.Errorf("duplicate periodic delivery = %+v", got)
			}
			seen[got] = true
		case <-timer.C:
			t.Fatalf("timed out waiting for periodic deliveries; got %v of %v", seen, want)
		}
	}
}
