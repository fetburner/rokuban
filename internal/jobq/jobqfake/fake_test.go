package jobqfake

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
)

func TestContract(t *testing.T) {
	contracttest.Run(t, func(_ *testing.T, registry *jobq.Registry, worker jobq.Worker[contracttest.Args], _ string) contracttest.Fixture {
		jobq.Register(registry, worker)
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

func TestCoalesceMergesOnlyIdenticalArgs(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{Coalesce: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls.Add(1)
		return nil
	}))
	same := fakeArgs{Value: "same", TimeoutMS: 2000, Coalesce: true}
	for i, want := range []bool{false, true} {
		got, err := queue.Enqueue(context.Background(), same)
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		if got.Coalesced != want {
			t.Errorf("identical Args enqueue %d Coalesced = %v, want %v", i, got.Coalesced, want)
		}
	}
	// One field differs, and it is not Value; the key covers the complete Args.
	other := same
	other.TimeoutMS = 3000
	got, err := queue.Enqueue(context.Background(), other)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if got.Coalesced {
		t.Error("Args differing in one field were coalesced")
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls = %d, want literal 2 (identical merged, different kept)", got)
	}
}

func TestInOrderUsesPriorityThenInsertion(t *testing.T) {
	queue := newQueue(t, Config{InOrder: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error { return nil }))
	enqueueABC(t, queue)
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := deliveredValues(queue); !slices.Equal(got, []string{"B", "A", "C"}) {
		t.Errorf("InOrder delivery = %v, want [B A C]", got)
	}
}

// TestKnobsKeepWeakestDefaults checks that no knob other than Coalesce and InOrder turns on
// coalescing or ordering, which the contract does not guarantee.
func TestKnobsKeepWeakestDefaults(t *testing.T) {
	zero := time.Duration(0)
	configs := map[string]Config{
		"zero":                  {},
		"DuplicateDelivery":     {DuplicateDelivery: true},
		"RedeliverAfterSuccess": {RedeliverAfterSuccess: true},
		"SnoozeDelay":           {SnoozeDelay: &zero},
		"NewIDEachDelivery":     {NewIDEachDelivery: true},
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			queue := newQueue(t, config, fakeWorkerFunc(func(context.Context, fakeArgs) error { return nil }))
			same := fakeArgs{Value: "same", TimeoutMS: 2000, Coalesce: true}
			for range 2 {
				got, err := queue.Enqueue(context.Background(), same)
				if err != nil {
					t.Fatalf("Enqueue() error = %v", err)
				}
				if got.Coalesced {
					t.Error("identical Args were coalesced without Config.Coalesce")
				}
			}
			startQueue(t, queue)
			waitQueue(t, queue)
			if got := len(queue.Deliveries()); got < 2 {
				t.Errorf("deliveries = %d, want at least literal 2 (no coalescing)", got)
			}
		})
		t.Run(name+" order", func(t *testing.T) {
			queue := newQueue(t, config, fakeWorkerFunc(func(context.Context, fakeArgs) error { return nil }))
			enqueueABC(t, queue)
			startQueue(t, queue)
			waitQueue(t, queue)
			if got := deliveredValues(queue); !slices.Equal(got, []string{"C", "B", "A"}) {
				t.Errorf("delivery order = %v, want newest first [C B A] ignoring priority", got)
			}
		})
	}
}

func TestNewIDEachDeliveryDoesNotCoalesce(t *testing.T) {
	var calls atomic.Int32
	queue := newQueue(t, Config{NewIDEachDelivery: true}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		calls.Add(1)
		return nil
	}))
	same := fakeArgs{Value: "same", TimeoutMS: 2000, Coalesce: true}
	for range 2 {
		if _, err := queue.Enqueue(context.Background(), same); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	startQueue(t, queue)
	waitQueue(t, queue)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls = %d, want literal 2", got)
	}
}

func TestWaitIdleWaitsForActiveWorker(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	queue := newQueue(t, Config{}, fakeWorkerFunc(func(context.Context, fakeArgs) error {
		close(entered)
		<-release
		close(completed)
		return nil
	}))
	if _, err := queue.Enqueue(context.Background(), fakeArgs{Value: "wait-idle", TimeoutMS: 2000}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	startQueue(t, queue)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := queue.WaitIdle(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("WaitIdle() error while worker is active = %v, want context deadline exceeded", err)
	}
	select {
	case <-completed:
		t.Fatal("worker completed before it was released")
	default:
	}

	close(release)
	waitQueue(t, queue)
	select {
	case <-completed:
	default:
		t.Fatal("WaitIdle() returned before the worker completed")
	}
}

// enqueueABC inserts A (priority 5), B (priority 1), C (priority 5). Priority order is
// B A C, insertion order A B C, and newest first C B A, so each policy gives a distinct result.
func enqueueABC(t *testing.T, queue *Queue) {
	t.Helper()
	for _, args := range []fakeArgs{
		{Value: "A", TimeoutMS: 2000, Priority: 5},
		{Value: "B", TimeoutMS: 2000, Priority: 1},
		{Value: "C", TimeoutMS: 2000, Priority: 5},
	} {
		if _, err := queue.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
}

// deliveredValues returns each Value once, in order of first delivery.
func deliveredValues(queue *Queue) []string {
	var values []string
	for _, delivery := range queue.Deliveries() {
		var args fakeArgs
		if err := json.Unmarshal(delivery.Args, &args); err != nil {
			panic(err)
		}
		if !slices.Contains(values, args.Value) {
			values = append(values, args.Value)
		}
	}
	return values
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
