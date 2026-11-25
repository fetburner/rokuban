package asynqadapter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
	"github.com/fetburner/rokuban/internal/jobs"
)

func TestNewAdapterValidatesOnceMode(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "unused"})
	t.Cleanup(func() { closeRedis(t, client) })
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(context.Context, contracttest.Args) error { return nil }))
	queue := jobq.QueueSpec{Logical: "default", Physical: []string{"default"}, MaxWorkers: 1}
	queueFor := func(jobq.Args) (string, error) { return "default", nil }
	for _, tc := range []struct {
		name     string
		queues   []jobq.QueueSpec
		schedule bool
		want     string
	}{
		{name: "valid", queues: []jobq.QueueSpec{queue}},
		{name: "no queues", want: "exactly one physical queue"},
		{name: "multiple logical queues", queues: []jobq.QueueSpec{queue, {Logical: "encode", Physical: []string{"encode"}, MaxWorkers: 1}}, want: "exactly one physical queue"},
		{name: "multiple sites", queues: []jobq.QueueSpec{{Logical: "ingest", Physical: []string{"ingest_tokyo", "ingest_osaka"}, MaxWorkers: 1}}, want: "exactly one physical queue"},
		{name: "parallel workers limited to one", queues: []jobq.QueueSpec{{Logical: "default", Physical: []string{"default"}, MaxWorkers: 2}}},
		{name: "periodic scheduling", queues: []jobq.QueueSpec{queue}, schedule: true, want: "periodic scheduling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Once: true, SchedulePeriodicJobs: tc.schedule, Spec: jobq.WorkerSpec{Queues: tc.queues}}
			if tc.name == "valid" || tc.schedule {
				cfg.Spec.PeriodicJobs = []jobq.PeriodicJobSpec{{Interval: time.Minute, Args: contracttest.Args{}}}
			}
			adapter, err := NewAdapter(client, registry, queueFor, cfg)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("NewAdapter() error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := adapter.queues["default"]; got != 1 {
				t.Errorf("server concurrency for default = %d, want 1", got)
			}
			if err := adapter.Start(t.Context()); err == nil {
				t.Fatal("Start accepted a one-task adapter")
			}
		})
	}

	adapter := testAdapter(t, client, registry, queueFor, Config{})
	if _, err := adapter.RunOnce(t.Context(), t.Context(), time.Second, func() {}); err == nil {
		t.Fatal("RunOnce accepted an adapter configured for ordinary delivery")
	}
}

type argsWithoutQueueSite struct{ contracttest.Args }

func (argsWithoutQueueSite) Kind() string    { return "missing_queue_site" }
func (argsWithoutQueueSite) Spec() jobq.Spec { return (jobs.IngestJobArgs{}).Spec() }

type workerWithoutQueueSite struct{}

func (workerWithoutQueueSite) Work(context.Context, argsWithoutQueueSite) error { return nil }

func TestNewAdapterChecksRegisteredQueueWithoutPeriodicJobs(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "unused"})
	t.Cleanup(func() { closeRedis(t, client) })
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(context.Context, contracttest.Args) error { return nil }))
	jobq.Register(registry, workerWithoutQueueSite{})

	_, err := NewAdapter(client, registry, jobs.PhysicalQueueFor, Config{})
	if err == nil || !strings.Contains(err.Error(), "missing_queue_site") || !strings.Contains(err.Error(), "QueueSite") {
		t.Fatalf("NewAdapter() error = %v, want missing_queue_site rejected for missing QueueSite", err)
	}
}

func TestRunOnceConsumesOnlyOneTask(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("once-one")
	inspector := testInspector(t, options, queue)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	defer releaseWork()
	var calls atomic.Int32
	adapter := redisOnceAdapter(t, options, queue, func(ctx context.Context, _ contracttest.Args) error {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	for range 2 {
		enqueueTestJob(t, adapter, contracttest.Args{TimeoutMS: 60_000})
	}

	run := startOnceTest(t, adapter, t.Context(), 15*time.Second)
	await(t, started, 10*time.Second)

	// Observe Stop while the first handler still holds the only slot. Checking
	// pending only after Shutdown would leave the Stop mutation to a race.
	pollOnceTest(t, 15*time.Second, "server to stop fetching during Work", func() bool {
		servers, err := inspector.Servers()
		if err != nil {
			t.Fatal(err)
		}
		for _, server := range servers {
			if _, ok := server.Queues[queue]; ok && server.Status == "stopped" {
				return true
			}
		}
		return false
	})

	releaseWork()
	result := run.wait(t)
	checkOnceResult(t, result, jobq.OnceOutcomeJobDone)
	info := onceQueueInfo(t, inspector, queue)
	if calls.Load() != 1 || info.Pending != 1 || info.Active != 0 || info.Retry != 0 {
		t.Fatalf("Work calls = %d, pending=%d active=%d retry=%d, want 1 Work and 1 pending task",
			calls.Load(), info.Pending, info.Active, info.Retry)
	}

	// RunOnce builds a fresh Server on the same Adapter after Shutdown.
	runOnceTest(t, adapter, 10*time.Second, jobq.OnceOutcomeJobDone)
	if info = onceQueueInfo(t, inspector, queue); calls.Load() != 2 || info.Size != 0 {
		t.Fatalf("after reuse: Work calls = %d, queue size = %d, want 2 calls and an empty queue", calls.Load(), info.Size)
	}
}

func TestRunOnceIdleTimeout(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("once-idle")
	testInspector(t, options, queue)
	adapter := redisOnceAdapter(t, options, queue, func(context.Context, contracttest.Args) error {
		t.Error("Work ran on an empty queue")
		return nil
	})
	runOnceTest(t, adapter, 100*time.Millisecond, jobq.OnceOutcomeIdleTimeout)
}

func TestRunOnceIdleTimeoutDoesNotCutRunningTask(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("once-running")
	testInspector(t, options, queue)
	const idleTimeout = 5 * time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	adapter := redisOnceAdapter(t, options, queue, func(ctx context.Context, _ contracttest.Args) error {
		close(started)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			t.Errorf("Work canceled at the idle deadline: %v", ctx.Err())
			return ctx.Err()
		}
	})
	enqueueTestJob(t, adapter, contracttest.Args{TimeoutMS: 30_000})
	releaseWork := sync.OnceFunc(func() { close(release) })
	defer releaseWork()
	run := startOnceTest(t, adapter, t.Context(), idleTimeout)
	await(t, started, idleTimeout)
	timer := time.NewTimer(2 * idleTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		releaseWork()
		checkOnceResult(t, run.wait(t), jobq.OnceOutcomeJobDone)
	case <-run.done:
		t.Fatalf("RunOnce returned while Work was still running: %+v", run.result)
	}
}

func TestRunOnceFailureRetries(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("once-retry")
	inspector := testInspector(t, options, queue)
	adapter := redisOnceAdapter(t, options, queue, func(context.Context, contracttest.Args) error {
		return errors.New("temporary Work failure")
	})
	enqueueTestJob(t, adapter, contracttest.Args{TimeoutMS: 10_000, MaxAttempts: 2})
	runOnceTest(t, adapter, 10*time.Second, jobq.OnceOutcomeJobDone)
	tasks, err := inspector.ListRetryTasks(queue)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListRetryTasks() = %d tasks, %v, want one retry", len(tasks), err)
	}
	if tasks[0].Retried != 1 || !strings.Contains(tasks[0].LastErr, "temporary Work failure") {
		t.Errorf("retry has attempts=%d error=%q, want one failed Work attempt", tasks[0].Retried, tasks[0].LastErr)
	}
}

func TestRunOnceUnhandledKind(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("once-unhandled")
	inspector := testInspector(t, options, queue)
	adapter := redisOnceAdapter(t, options, queue, func(context.Context, contracttest.Args) error {
		t.Error("Work ran for an unregistered kind")
		return nil
	})
	if _, err := adapter.client.EnqueueContext(t.Context(), asynq.NewTask("unregistered", []byte("{}")), asynq.Queue(queue)); err != nil {
		t.Fatal(err)
	}
	runOnceTest(t, adapter, 10*time.Second, jobq.OnceOutcomeJobUnhandled)
	if info := onceQueueInfo(t, inspector, queue); info.Size != 0 {
		t.Errorf("queue size = %d after handling unknown kind, want empty", info.Size)
	}
}

func TestRunOncePromotesDueSnoozedTask(t *testing.T) {
	options := redisOptions(t)
	queue, other := testQueue("once-snooze"), testQueue("once-other")
	inspector := testInspector(t, options, queue, other)
	var calls atomic.Int32
	adapter := redisOnceAdapter(t, options, queue, func(context.Context, contracttest.Args) error {
		if calls.Add(1) == 1 {
			return jobq.Snooze(5 * time.Second)
		}
		return nil
	})
	otherAdapter := redisOnceAdapter(t, options, other, func(context.Context, contracttest.Args) error {
		return jobq.Snooze(5 * time.Second)
	})
	var due time.Time
	for _, candidate := range []*Adapter{adapter, otherAdapter} {
		enqueueTestJob(t, candidate, contracttest.Args{TimeoutMS: 10_000, MaxAttempts: 1})
		runOnceTest(t, candidate, 10*time.Second, jobq.OnceOutcomeJobDone)
		for physical := range candidate.queues {
			tasks, err := inspector.ListRetryTasks(physical)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("%s: ListRetryTasks() = %d tasks, %v, want one snoozed task", physical, len(tasks), err)
			}
			if tasks[0].Retried != 0 {
				t.Errorf("%s: Snooze used %d attempts, want zero", physical, tasks[0].Retried)
			}
			if tasks[0].NextProcessAt.After(due) {
				due = tasks[0].NextProcessAt
			}
		}
	}

	timer := time.NewTimer(time.Until(due))
	defer timer.Stop()
	<-timer.C
	runOnceTest(t, adapter, 10*time.Second, jobq.OnceOutcomeJobDone)
	if info := onceQueueInfo(t, inspector, queue); calls.Load() != 2 || info.Size != 0 {
		t.Fatalf("after due Snooze: Work calls = %d, queue size = %d, want 2 calls and an empty queue", calls.Load(), info.Size)
	}
	if info := onceQueueInfo(t, inspector, other); info.Retry != 1 || info.Pending != 0 {
		t.Errorf("another queue: retry=%d pending=%d, want one retry and no pending task", info.Retry, info.Pending)
	}
}

// TestRunOnceRecoversOrphanedTask は RunOnce の server が孤児を回収して実行することを確かめる。
// 回収を待つので t.Parallel で重ねる。根拠は TestCrashExhaustsOnlyAttempt の doc comment と同じ。
// RunOnce の server は自分の物理キュー 1 本だけを購読するため、別テストのキューを回収しない。
func TestRunOnceRecoversOrphanedTask(t *testing.T) {
	t.Parallel()
	options := redisOptions(t)
	var calls atomic.Int32
	started := make(chan struct{})
	worker := testWorker(func(ctx context.Context, _ contracttest.Args) error {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	registry := jobq.NewRegistry()
	jobq.Register(registry, worker)
	fixture := newRedisFixture(t, options, registry, "once-orphan")
	enqueueTestJob(t, fixture.Adapter, contracttest.Args{TimeoutMS: 30_000, MaxAttempts: 2})
	startTestAdapter(t, fixture.Adapter)
	await(t, started, 10*time.Second)
	crashCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := fixture.Crash(crashCtx); err != nil {
		t.Fatal(err)
	}

	pollOnceTest(t, RecoveryGrace, "active task's lease to expire", func() bool {
		tasks, err := fixture.inspector.ListActiveTasks(fixture.queue)
		if err != nil {
			t.Fatal(err)
		}
		return len(tasks) == 1 && tasks[0].IsOrphaned
	})
	// There is no other running server. Allow for asynq's grace after lease
	// expiration and its recovery sweep before treating the run as idle.
	adapter := redisOnceAdapter(t, options, fixture.queue, worker)
	runOnceTest(t, adapter, RecoveryGrace, jobq.OnceOutcomeJobDone)
	if info := onceQueueInfo(t, fixture.inspector, fixture.queue); calls.Load() != 2 || info.Size != 0 {
		t.Fatalf("after recovery: Work calls = %d, queue size = %d, want 2 calls and an empty queue", calls.Load(), info.Size)
	}
}

func TestRunOnceCanceled(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("once-canceled")
	testInspector(t, options, queue)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Keep Work in flight until the cancellation path starts draining.
	workStarted := make(chan struct{})
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	defer releaseWork()
	adapter := redisOnceAdapter(t, options, queue, func(context.Context, contracttest.Args) error {
		close(workStarted)
		<-release
		return nil
	})
	enqueueTestJob(t, adapter, contracttest.Args{TimeoutMS: 30_000})
	run := startOnceTest(t, adapter, ctx, 10*time.Second)
	await(t, workStarted, 10*time.Second)
	cancel()
	pollOnceTest(t, 10*time.Second, "RunOnce to begin draining", func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return adapter.run != nil && adapter.run.stopping
	})
	releaseWork()
	checkOnceResult(t, run.wait(t), jobq.OnceOutcomeCanceled)
}

type onceResult struct {
	outcome jobq.OnceOutcome
	err     error
}

type onceTestRun struct {
	done   chan struct{}
	result onceResult
}

func startOnceTest(t *testing.T, adapter *Adapter, ctx context.Context, idleTimeout time.Duration) *onceTestRun {
	t.Helper()
	run := &onceTestRun{done: make(chan struct{})}
	go func() {
		defer close(run.done)
		outcome, err := adapter.RunOnce(ctx, t.Context(), idleTimeout, func() {})
		run.result = onceResult{outcome, err}
	}()
	t.Cleanup(func() { run.wait(t) })
	return run
}

func (run *onceTestRun) wait(t *testing.T) onceResult {
	t.Helper()
	await(t, run.done, 10*time.Second)
	return run.result
}

func runOnceTest(t *testing.T, adapter *Adapter, idleTimeout time.Duration, want jobq.OnceOutcome) {
	t.Helper()
	processCanceled := false
	outcome, err := adapter.RunOnce(t.Context(), t.Context(), idleTimeout, func() {
		processCanceled = true
	})
	checkOnceResult(t, onceResult{outcome, err}, want)
	if !processCanceled {
		t.Error("RunOnce did not call cancelProcess")
	}
}

func checkOnceResult(t *testing.T, result onceResult, want jobq.OnceOutcome) {
	t.Helper()
	if result.err != nil || result.outcome != want {
		t.Fatalf("RunOnce() = %s, %v, want %s, nil", result.outcome, result.err, want)
	}
}

func redisOnceAdapter(t *testing.T, options *redis.Options, queue string, worker testWorker) *Adapter {
	t.Helper()
	cfg := Config{Once: true, Spec: jobq.WorkerSpec{Queues: []jobq.QueueSpec{{
		Logical: "default", Physical: []string{queue}, MaxWorkers: 1,
	}}}}
	return redisTestAdapter(t, options, func(jobq.Args) (string, error) { return queue, nil }, cfg, worker)
}

func onceQueueInfo(t *testing.T, inspector *asynq.Inspector, queue string) *asynq.QueueInfo {
	t.Helper()
	info, err := inspector.GetQueueInfo(queue)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func pollOnceTest(t *testing.T, limit time.Duration, description string, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}
