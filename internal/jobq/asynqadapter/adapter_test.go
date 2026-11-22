package asynqadapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
)

type testWorker func(context.Context, contracttest.Args) error

// Work はテストが指定した処理を呼ぶ。
func (work testWorker) Work(ctx context.Context, args contracttest.Args) error {
	return work(ctx, args)
}

func TestRetryDelayUsesFailedAttemptNumber(t *testing.T) {
	failure := errors.New("temporary failure")
	for _, tc := range []struct {
		retries int
		want    time.Duration
	}{
		{0, time.Second},
		{1, 16 * time.Second},
		{2, 81 * time.Second},
		{309, 9_223_372_036_854_775_807},
	} {
		if got := retryDelay(tc.retries, failure, nil); got != tc.want {
			t.Errorf("retryDelay(%d) = %s, want %s", tc.retries, got, tc.want)
		}
	}
}

func TestNewAdapterValidatesWorkerSpec(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "unused"})
	t.Cleanup(func() { closeRedis(t, client) })
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(context.Context, contracttest.Args) error { return nil }))
	validQueue := jobq.QueueSpec{Logical: "default", Physical: []string{"default"}, MaxWorkers: 1}
	validJob := jobq.PeriodicJobSpec{Interval: time.Second, Args: contracttest.Args{}}
	validQueueFor := func(jobq.Args) (string, error) { return "default", nil }
	for _, tc := range []struct {
		name    string
		queue   jobq.QueueSpec
		job     jobq.PeriodicJobSpec
		reg     *jobq.Registry
		resolve QueueFunc
		want    string
	}{
		{"valid", validQueue, validJob, registry, validQueueFor, ""},
		{"missing queue resolver", validQueue, validJob, registry, nil, "queueFor is required"},
		{"zero workers", jobq.QueueSpec{Logical: "default", Physical: []string{"default"}}, validJob, registry, validQueueFor, "MaxWorkers"},
		{"zero period", validQueue, jobq.PeriodicJobSpec{Args: contracttest.Args{}}, registry, validQueueFor, "interval must be positive"},
		{"unknown kind", validQueue, validJob, jobq.NewRegistry(), validQueueFor, "not resolvable"},
		{"mismatched queue", validQueue, validJob, registry, func(jobq.Args) (string, error) { return "elsewhere", nil }, "nobody would pull it"},
		{"no physical queues", jobq.QueueSpec{Logical: "default", MaxWorkers: 1}, validJob, registry, validQueueFor, "no physical queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Spec: jobq.WorkerSpec{Queues: []jobq.QueueSpec{tc.queue}, PeriodicJobs: []jobq.PeriodicJobSpec{tc.job}}}
			_, err := NewAdapter(client, tc.reg, tc.resolve, cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("NewAdapter() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewAdapter() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPhysicalQueuesHaveIndependentConcurrency(t *testing.T) {
	options := redisOptions(t)
	narrow, wide := testQueue("narrow"), testQueue("wide")
	testInspector(t, options, narrow, wide)
	release := make(chan struct{})
	defer close(release)
	narrowStarted, wideStarted := make(chan struct{}, 4), make(chan struct{}, 4)
	queues := map[string]string{"small": narrow, "large": wide}
	queueFor := func(args jobq.Args) (string, error) { return queues[args.Spec().Queue], nil }
	cfg := Config{Spec: jobq.WorkerSpec{Queues: []jobq.QueueSpec{
		{Logical: "small", Physical: []string{narrow}, MaxWorkers: 1},
		{Logical: "large", Physical: []string{wide}, MaxWorkers: 2},
	}}}
	adapter := redisTestAdapter(t, options, queueFor, cfg, func(ctx context.Context, args contracttest.Args) error {
		if args.Queue == "small" {
			narrowStarted <- struct{}{}
		} else {
			wideStarted <- struct{}{}
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	for range 4 {
		enqueueTestJob(t, adapter, contracttest.Args{Queue: "small", TimeoutMS: 15000})
	}
	startTestAdapter(t, adapter)
	await(t, narrowStarted, 4*time.Second)
	select {
	case <-narrowStarted:
		t.Fatal("small queue started more than one Work")
	case <-time.After(2 * time.Second):
	}
	for range 4 {
		enqueueTestJob(t, adapter, contracttest.Args{Queue: "large", TimeoutMS: 15000})
	}
	for range 2 {
		await(t, wideStarted, 3*time.Second)
	}
	select {
	case <-narrowStarted:
		t.Fatal("small queue exceeded one concurrent Work")
	case <-wideStarted:
		t.Fatal("large queue exceeded two concurrent Work")
	case <-time.After(2 * time.Second):
	}
}

func TestPeriodicJobs(t *testing.T) {
	options := redisOptions(t)
	for _, schedule := range []bool{true, false} {
		name := "disabled"
		if schedule {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			queue := testQueue("periodic")
			testInspector(t, options, queue)
			queueFor := func(jobq.Args) (string, error) { return queue, nil }
			deliveries := make(chan time.Time, 10)
			manual := make(chan struct{}, 1)
			cfg := Config{SchedulePeriodicJobs: schedule, Spec: jobq.WorkerSpec{
				Queues: []jobq.QueueSpec{{Logical: "default", Physical: []string{queue}, MaxWorkers: 1}},
				PeriodicJobs: []jobq.PeriodicJobSpec{{
					Interval: 3 * time.Second, Args: contracttest.Args{Value: "periodic", TimeoutMS: 2000},
				}},
			}}
			adapter := redisTestAdapter(t, options, queueFor, cfg, func(_ context.Context, args contracttest.Args) error {
				if args.Value == "periodic" {
					deliveries <- time.Now()
				} else {
					manual <- struct{}{}
				}
				return nil
			})
			startedAt := time.Now()
			startTestAdapter(t, adapter)
			if schedule {
				first := await(t, deliveries, 2500*time.Millisecond)
				if first.Sub(startedAt) >= 3*time.Second {
					t.Fatalf("first periodic delivery = %s after Start, want before 3s", first.Sub(startedAt))
				}
				second := await(t, deliveries, 5*time.Second)
				if second.Sub(startedAt) < 3*time.Second {
					t.Fatalf("second periodic delivery = %s after Start, want at least 3s", second.Sub(startedAt))
				}
				return
			}
			enqueueTestJob(t, adapter, contracttest.Args{Value: "manual", TimeoutMS: 2000})
			await(t, manual, 4*time.Second)
			select {
			case <-deliveries:
				t.Fatal("disabled periodic job was delivered")
			case <-time.After(5 * time.Second):
			}
		})
	}
}

func TestStopDrainsWorkAndStartRestartsDelivery(t *testing.T) {
	options := redisOptions(t)
	queue := testQueue("restart")
	testInspector(t, options, queue)
	cfg := Config{Spec: jobq.WorkerSpec{Queues: []jobq.QueueSpec{{
		Logical: "default", Physical: []string{queue}, MaxWorkers: 1,
	}}}}
	first := make(chan struct{}, 1)
	second := make(chan struct{}, 1)
	release := make(chan struct{})
	adapter := redisTestAdapter(t, options, func(jobq.Args) (string, error) { return queue, nil }, cfg,
		func(ctx context.Context, args contracttest.Args) error {
			if args.Value == "second" {
				second <- struct{}{}
				return nil
			}
			first <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	startTestAdapter(t, adapter)
	enqueueTestJob(t, adapter, contracttest.Args{Value: "first", TimeoutMS: 10000})
	await(t, first, 4*time.Second)
	stopCtx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- adapter.Stop(stopCtx) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop completed before Work was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := await(t, stopped, 5*time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	enqueueTestJob(t, adapter, contracttest.Args{Value: "second", TimeoutMS: 2000})
	startTestAdapter(t, adapter)
	await(t, second, 4*time.Second)
}

func redisTestAdapter(t *testing.T, options *redis.Options, queueFor QueueFunc, cfg Config, work testWorker) *Adapter {
	t.Helper()
	registry := jobq.NewRegistry()
	jobq.Register(registry, work)
	connection := redis.NewClient(options)
	t.Cleanup(func() { closeRedis(t, connection) })
	adapter := testAdapter(t, connection, registry, queueFor, cfg)
	t.Cleanup(func() { stopTestAdapter(t, adapter) })
	return adapter
}

func testAdapter(t *testing.T, connection *redis.Client, registry *jobq.Registry, queueFor QueueFunc, cfg Config) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(connection, registry, queueFor, cfg)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	return adapter
}

func startTestAdapter(t *testing.T, adapter *Adapter) {
	t.Helper()
	if err := adapter.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

func stopTestAdapter(t *testing.T, adapter *Adapter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adapter.Stop(ctx); err != nil {
		t.Errorf("Stop() during cleanup: %v", err)
	}
}

func enqueueTestJob(t *testing.T, adapter *Adapter, args jobq.Args) {
	t.Helper()
	if _, err := adapter.Enqueue(t.Context(), args); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
}

func await[T any](t *testing.T, ch <-chan T, limit time.Duration) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(limit):
		t.Fatal("timed out waiting for adapter")
		panic("unreachable")
	}
}
