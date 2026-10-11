package jobqfake

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
)

func TestContract(t *testing.T) {
	contracttest.Run(t, func(_ *testing.T, registry *jobq.Registry) contracttest.Fixture {
		return New(registry, Config{})
	})
}

func TestDuplicateDeliveryDeliversTwiceWithSameID(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{DuplicateDelivery: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		if calls.Add(1) == 1 {
			return errors.New("retryable first delivery")
		}
		return nil
	}))
	startQueue(t, queue)
	if _, err := queue.Enqueue(context.Background(), fakeArgs{Value: "twice", TimeoutMS: 2000, MaxAttempts: 1}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Fatalf("Work calls = %d, want literal 2", got)
	}
	deliveries := queue.Deliveries()
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %d, want literal 2", len(deliveries))
	}
	if deliveries[0].JobID != deliveries[1].JobID {
		t.Errorf("duplicate IDs = %q and %q, want same ID", deliveries[0].JobID, deliveries[1].JobID)
	}
}

func TestNoCoalesceAllowsDuplicateJobs(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{NoCoalesce: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls.Add(1)
		return nil
	}))
	args := fakeArgs{Value: "same", TimeoutMS: 2000, Coalesce: true}
	for range 2 {
		if _, err := queue.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls with NoCoalesce = %d, want literal 2", got)
	}
}

func TestDefaultConfigDoesNotCoalesce(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls.Add(1)
		return nil
	}))
	args := fakeArgs{Value: "same", TimeoutMS: 2000, Coalesce: true}
	for range 2 {
		if _, err := queue.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls with default Config = %d, want literal 2", got)
	}
}

func TestReorderIgnoresPriorityAndUsesReverseInsertion(t *testing.T) {
	calls := make(chan string, 2)
	queue := newQueue(t, Config{Reorder: true}, fakeWorkerFunc(func(_ context.Context, args fakeArgs) error {
		calls <- args.Value
		return nil
	}))
	for _, args := range []fakeArgs{
		{Value: "higher priority, first", TimeoutMS: 2000, Priority: 1},
		{Value: "lower priority, second", TimeoutMS: 2000, Priority: 5},
	} {
		if _, err := queue.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := receive(t, calls); got != "lower priority, second" {
		t.Errorf("first Work = %q, want last enqueue despite higher priority hint", got)
	}
	if got := receive(t, calls); got != "higher priority, first" {
		t.Errorf("second Work = %q, want first enqueue", got)
	}
}

func TestDefaultConfigIgnoresOrderingHints(t *testing.T) {
	calls := make(chan string, 2)
	queue := newQueue(t, Config{}, fakeWorkerFunc(func(_ context.Context, args fakeArgs) error {
		calls <- args.Value
		return nil
	}))
	for _, args := range []fakeArgs{
		{Value: "higher priority, first", TimeoutMS: 2000, Priority: 1},
		{Value: "lower priority, second", TimeoutMS: 2000, Priority: 5},
	} {
		if _, err := queue.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := receive(t, calls); got != "lower priority, second" {
		t.Errorf("first Work with default Config = %q, want newest enqueue", got)
	}
	if got := receive(t, calls); got != "higher priority, first" {
		t.Errorf("second Work with default Config = %q, want oldest enqueue", got)
	}
}

func TestRedeliverAfterSuccessDeliversTwice(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{RedeliverAfterSuccess: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls.Add(1)
		return nil
	}))
	startQueue(t, queue)
	if _, err := queue.Enqueue(context.Background(), fakeArgs{TimeoutMS: 2000}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls after success = %d, want literal 2", got)
	}
}

func TestSnoozeDelayZeroIgnoresRequestedDelay(t *testing.T) {
	zero := time.Duration(0)
	calls := make(chan time.Time, 2)
	var count atomic.Int32
	queue := newQueue(t, Config{SnoozeDelay: &zero}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls <- time.Now()
		if count.Add(1) == 1 {
			return jobq.Snooze(10 * time.Second)
		}
		return nil
	}))
	startQueue(t, queue)
	if _, err := queue.Enqueue(context.Background(), fakeArgs{TimeoutMS: 2000, MaxAttempts: 1}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitQueue(t, queue)
	first, second := receive(t, calls), receive(t, calls)
	if elapsed := second.Sub(first); elapsed >= 3*time.Second {
		t.Errorf("delay with zero override = %s, want less than 3s", elapsed)
	}
}

func TestNewIDEachDeliveryChangesID(t *testing.T) {
	queue := newQueue(t, Config{DuplicateDelivery: true, NewIDEachDelivery: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		return nil
	}))
	startQueue(t, queue)
	if _, err := queue.Enqueue(context.Background(), fakeArgs{TimeoutMS: 2000}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitQueue(t, queue)
	deliveries := queue.Deliveries()
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %d, want literal 2", len(deliveries))
	}
	if deliveries[0].JobID == deliveries[1].JobID {
		t.Errorf("delivery ID remained %q; NewIDEachDelivery did not take effect", deliveries[0].JobID)
	}
}

func TestCoalescingUsesCompleteArgs(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls.Add(1)
		return nil
	}))
	for _, args := range []fakeArgs{
		{Value: "first", TimeoutMS: 2000, Coalesce: true},
		{Value: "second", TimeoutMS: 2000, Coalesce: true},
	} {
		if _, err := queue.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls for distinct Args = %d, want literal 2", got)
	}
}

type fakeArgs struct {
	Value       string
	TimeoutMS   int64
	MaxAttempts int
	Coalesce    bool
	Priority    int
}

func (fakeArgs) Kind() string { return "fake" }

func (args fakeArgs) Spec() jobq.Spec {
	return jobq.Spec{
		Queue: "fake",
		Timeout: func(raw jobq.Args) time.Duration {
			args, ok := raw.(fakeArgs)
			if !ok {
				return 0
			}
			if args.TimeoutMS <= 0 {
				return time.Hour
			}
			return time.Duration(args.TimeoutMS) * time.Millisecond
		},
		MaxAttempts: args.MaxAttempts,
		Coalesce:    args.Coalesce,
		Priority:    args.Priority,
	}
}

type fakeWorkerFunc func(context.Context, fakeArgs) error

func (worker fakeWorkerFunc) Work(ctx context.Context, args fakeArgs) error {
	return worker(ctx, args)
}

func newQueue(t *testing.T, config Config, worker fakeWorkerFunc) *Queue {
	t.Helper()
	registry := jobq.NewRegistry()
	jobq.Register[fakeArgs](registry, worker)
	return New(registry, config)
}

func startQueue(t *testing.T, queue *Queue) {
	t.Helper()
	if err := queue.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = queue.Stop(ctx)
	})
}

func waitQueue(t *testing.T, queue *Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := queue.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}
