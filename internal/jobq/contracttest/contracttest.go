// Package contracttest contains the shared tests every jobq adapter must pass.
package contracttest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
)

// Fixture combines an adapter with the test controls needed to observe delivery and transactions.
type Fixture interface {
	jobq.Enqueuer
	jobq.Runner
	WaitIdle(context.Context) error
	BeginTx(context.Context) (db.Tx, error)
	Crash(context.Context) error
	// RecoveryGrace is the adapter's recovery grace in jobq.Spec.Timeout: how long after the
	// timeout it may take to make a job whose process died eligible for redelivery.
	RecoveryGrace() time.Duration
}

// Factory registers worker in registry and creates an isolated adapter fixture for queueName.
// Passing the queue lets the shared contract exercise the maximum supported name while each
// fixture processes the queue under test; a default-only fixture could accept an enqueue without
// ever delivering it.
type Factory func(t *testing.T, registry *jobq.Registry, worker jobq.Worker[Args], queueName string) Fixture

// Run executes the common adapter contract tests against factory.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"enqueue decodes the JSON round trip", testEnqueueRoundTrip},
		{"accepts a queue name at the contract maximum", testQueueNameMaxLen},
		{"EnqueueTx waits for commit and disappears on rollback", testEnqueueTx},
		{"MaxAttempts stops after the literal limit", testMaxAttempts},
		{"ErrDiscard is not delivered again", testErrDiscard},
		{"Snooze does not consume MaxAttempts", testSnoozeAttempts},
		{"Snooze delay is a lower bound", testSnoozeDelay},
		{"timeout cancels Work and retries it", testTimeout},
		{"timeout takes precedence over Snooze", testTimeoutOverridesSnooze},
		{"Stop cancels overdue Work and retries it", testStop},
		{"process death makes active Work eligible again", testCrash},
		{"coalescing accepts one or more deliveries", testCoalescing},
		{"Register rejects a nonpositive timeout", testRegisterTimeout},
		{"Enqueue rejects a nonpositive timeout", testEnqueueTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.run(t, factory) })
	}
}

func testQueueNameMaxLen(t *testing.T, factory Factory) {
	// Lowercase letters, digits, "_" and "-" all appear, the length is exactly the maximum, and
	// the name ends with an alphanumeric character, as the jobq.MaxQueueNameLen doc guarantees.
	queueName := strings.Repeat("a1_b2-", jobq.MaxQueueNameLen)[:jobq.MaxQueueNameLen]
	queueName = strings.TrimRight(queueName, "_-")
	queueName += strings.Repeat("z", jobq.MaxQueueNameLen-len(queueName))
	args := contractArgs{Queue: queueName, Value: "maximum queue name", TimeoutMS: 2000, MaxAttempts: 1}
	if got := args.Spec().Queue; got != queueName {
		t.Fatalf("Spec().Queue = %q, want %q", got, queueName)
	}
	calls := make(chan contractArgs, 1)
	fixture := newFixtureForQueue(t, factory, contractWorker{work: func(_ context.Context, got contractArgs) error {
		calls <- got
		return nil
	}}, queueName)
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), args); err != nil {
		t.Fatalf("Enqueue() with %d-character queue name error = %v", len(queueName), err)
	}
	waitIdle(t, fixture)
	if got := receive(t, calls); !reflect.DeepEqual(got, args) {
		t.Errorf("decoded args = %#v, want %#v", got, args)
	}
}

func testEnqueueRoundTrip(t *testing.T, factory Factory) {
	args := contractArgs{Value: "round trip", Values: []int{3, 1, 4}, TimeoutMS: 2000, MaxAttempts: 2}
	calls := make(chan contractArgs, 2)
	fixture := newFixture(t, factory, contractWorker{work: func(_ context.Context, got contractArgs) error {
		calls <- got
		return nil
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), args); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitIdle(t, fixture)
	if got := receive(t, calls); !reflect.DeepEqual(got, args) {
		t.Errorf("decoded args = %#v, want %#v", got, args)
	}
}

func testEnqueueTx(t *testing.T, factory Factory) {
	calls := make(chan contractArgs, 4)
	fixture := newFixture(t, factory, contractWorker{work: func(_ context.Context, args contractArgs) error {
		calls <- args
		return nil
	}})
	start(t, fixture)
	args := contractArgs{Value: "transaction", TimeoutMS: 2000}

	rolledBack, err := fixture.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := fixture.EnqueueTx(context.Background(), rolledBack, args); err != nil {
		t.Fatalf("EnqueueTx(rollback) error = %v", err)
	}
	waitIdle(t, fixture)
	assertNoCall(t, calls)
	if err := rolledBack.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	waitIdle(t, fixture)
	assertNoCall(t, calls)

	committed, err := fixture.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := fixture.EnqueueTx(context.Background(), committed, args); err != nil {
		t.Fatalf("EnqueueTx(commit) error = %v", err)
	}
	waitIdle(t, fixture)
	assertNoCall(t, calls)
	if err := committed.Commit(context.Background()); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	waitIdle(t, fixture)
	if got := receive(t, calls); got.Value != args.Value {
		t.Errorf("committed args = %#v, want value %q", got, args.Value)
	}
	assertNoCall(t, calls)
}

func testMaxAttempts(t *testing.T, factory Factory) {
	var calls atomic.Int32
	fixture := newFixture(t, factory, contractWorker{work: func(context.Context, contractArgs) error {
		calls.Add(1)
		return errors.New("retry me")
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 2000, MaxAttempts: 2}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitIdle(t, fixture)
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls = %d, want literal 2", got)
	}
}

func testErrDiscard(t *testing.T, factory Factory) {
	var calls atomic.Int32
	fixture := newFixture(t, factory, contractWorker{work: func(context.Context, contractArgs) error {
		calls.Add(1)
		return jobq.ErrDiscard
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 2000, MaxAttempts: 3}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitIdle(t, fixture)
	if got := calls.Load(); got != 1 {
		t.Errorf("Work calls = %d, want literal 1", got)
	}
}

func testSnoozeAttempts(t *testing.T, factory Factory) {
	var calls atomic.Int32
	fixture := newFixture(t, factory, contractWorker{work: func(context.Context, contractArgs) error {
		if calls.Add(1) <= 3 {
			return jobq.Snooze(0)
		}
		return nil
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 2000, MaxAttempts: 1}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitIdle(t, fixture)
	if got := calls.Load(); got != 4 {
		t.Errorf("Work calls = %d, want literal 4 after three snoozes", got)
	}
}

func testSnoozeDelay(t *testing.T, factory Factory) {
	type delivery struct {
		args contractArgs
		at   time.Time
	}
	calls := make(chan delivery, 2)
	var callCount atomic.Int32
	fixture := newFixture(t, factory, contractWorker{work: func(_ context.Context, args contractArgs) error {
		calls <- delivery{args: args, at: time.Now()}
		if callCount.Add(1) == 1 {
			return jobq.Snooze(2 * time.Second)
		}
		return nil
	}})
	start(t, fixture)
	args := contractArgs{Value: "preserve on snooze", TimeoutMS: 5000, MaxAttempts: 1}
	if _, err := fixture.Enqueue(context.Background(), args); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitIdle(t, fixture)
	first, second := receive(t, calls), receive(t, calls)
	if elapsed := second.at.Sub(first.at); elapsed < 2*time.Second {
		t.Errorf("time between snoozed deliveries = %s, want at least 2s", elapsed)
	}
	if !reflect.DeepEqual(first.args, args) || !reflect.DeepEqual(second.args, args) {
		t.Errorf("snoozed Args changed: first=%#v second=%#v want=%#v", first.args, second.args, args)
	}
}

func testTimeout(t *testing.T, factory Factory) {
	calls := make(chan error, 3)
	fixture := newFixture(t, factory, contractWorker{work: func(ctx context.Context, _ contractArgs) error {
		<-ctx.Done()
		calls <- ctx.Err()
		return nil // A timeout still fails even if Work returns nil after its deadline.
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 20, MaxAttempts: 2}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	limit := redeliveryLimit(fixture, 20*time.Millisecond)
	waitIdleWithin(t, fixture, limit)
	for range 2 {
		if got := receiveWithin(t, calls, limit); !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("Work context error = %v, want DeadlineExceeded", got)
		}
	}
}

func testTimeoutOverridesSnooze(t *testing.T, factory Factory) {
	var calls atomic.Int32
	contexts := make(chan error, 1)
	fixture := newFixture(t, factory, contractWorker{work: func(ctx context.Context, _ contractArgs) error {
		if calls.Add(1) != 1 {
			return nil
		}
		<-ctx.Done()
		contexts <- ctx.Err()
		return jobq.Snooze(20 * time.Millisecond)
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 20, MaxAttempts: 1}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitIdle(t, fixture)
	if got := receive(t, contexts); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("Work context error = %v, want DeadlineExceeded", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Work calls after timed-out Snooze = %d, want literal 1", got)
	}
}

func testStop(t *testing.T, factory Factory) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	contexts := make(chan error, 2)
	fixture := newFixture(t, factory, contractWorker{work: func(ctx context.Context, _ contractArgs) error {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			contexts <- ctx.Err()
			return ctx.Err()
		}
		return nil
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 5000, MaxAttempts: 2}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	receive(t, started)
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	stopErr := fixture.Stop(stopCtx)
	cancel()
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Errorf("Stop() error = %v, want DeadlineExceeded after grace expires", stopErr)
	}
	if got := receive(t, contexts); !errors.Is(got, context.Canceled) {
		t.Errorf("stopped Work context error = %v, want Canceled", got)
	}
	start(t, fixture)
	waitIdleWithin(t, fixture, redeliveryLimit(fixture, 5*time.Second))
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls after Stop = %d, want literal 2", got)
	}
}

func testCrash(t *testing.T, factory Factory) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	contexts := make(chan error, 2)
	fixture := newFixture(t, factory, contractWorker{work: func(ctx context.Context, _ contractArgs) error {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			contexts <- ctx.Err()
			return ctx.Err()
		}
		return nil
	}})
	start(t, fixture)
	if _, err := fixture.Enqueue(context.Background(), contractArgs{TimeoutMS: 5000, MaxAttempts: 2}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	receive(t, started)
	crashCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := fixture.Crash(crashCtx); err != nil {
		t.Fatalf("Crash() error = %v", err)
	}
	cancel()
	if got := receive(t, contexts); !errors.Is(got, context.Canceled) {
		t.Errorf("crashed Work context error = %v, want Canceled", got)
	}
	start(t, fixture)
	waitIdleWithin(t, fixture, redeliveryLimit(fixture, 5*time.Second))
	if got := calls.Load(); got != 2 {
		t.Errorf("Work calls after crash = %d, want literal 2", got)
	}
}

func testCoalescing(t *testing.T, factory Factory) {
	var calls atomic.Int32
	fixture := newFixture(t, factory, contractWorker{work: func(context.Context, contractArgs) error {
		calls.Add(1)
		return nil
	}})
	args := contractArgs{Value: "same", TimeoutMS: 2000, Coalesce: true}
	for range 2 {
		if _, err := fixture.Enqueue(context.Background(), args); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}
	start(t, fixture)
	waitIdle(t, fixture)
	if got := calls.Load(); got < 1 {
		t.Errorf("Work calls = %d, want at least literal 1", got)
	}
}

func testRegisterTimeout(t *testing.T, _ Factory) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic for Timeout <= 0")
		}
	}()
	registry := jobq.NewRegistry()
	jobq.Register[invalidArgs](registry, invalidWorker{})
}

func testEnqueueTimeout(t *testing.T, factory Factory) {
	fixture := newFixture(t, factory, contractWorker{work: func(context.Context, contractArgs) error { return nil }})
	_, err := fixture.Enqueue(context.Background(), contractArgs{
		Value: "invalid timeout", TimeoutMS: 2000, InvalidTimeout: true,
	})
	if err == nil {
		t.Fatal("Enqueue accepted Spec.Timeout(args) <= 0")
	}
}

type contractArgs struct {
	Queue          string
	Value          string
	Values         []int
	TimeoutMS      int64
	MaxAttempts    int
	Coalesce       bool
	Priority       int
	InvalidTimeout bool
}

// Args is the job argument type supplied to adapter factories by the shared tests.
type Args = contractArgs

func (contractArgs) Kind() string { return "contracttest" }

func (args contractArgs) Spec() jobq.Spec {
	queue := args.Queue
	if queue == "" {
		queue = "default"
	}
	return jobq.Spec{
		Queue: queue,
		Timeout: func(raw jobq.Args) time.Duration {
			args, ok := raw.(contractArgs)
			if !ok {
				return 0
			}
			if args.TimeoutMS <= 0 {
				return time.Hour
			}
			if args.InvalidTimeout {
				return 0
			}
			return time.Duration(args.TimeoutMS) * time.Millisecond
		},
		MaxAttempts: args.MaxAttempts,
		Coalesce:    args.Coalesce,
		Priority:    args.Priority,
	}
}

type invalidArgs struct{}

func (invalidArgs) Kind() string { return "invalid_timeout" }
func (invalidArgs) Spec() jobq.Spec {
	return jobq.Spec{Timeout: func(jobq.Args) time.Duration { return 0 }}
}

type invalidWorker struct{}

func (invalidWorker) Work(context.Context, invalidArgs) error { return nil }

type contractWorker struct {
	work func(context.Context, contractArgs) error
}

func (worker contractWorker) Work(ctx context.Context, args contractArgs) error {
	return worker.work(ctx, args)
}

func newFixture(t *testing.T, factory Factory, worker contractWorker) Fixture {
	return newFixtureForQueue(t, factory, worker, "default")
}

func newFixtureForQueue(t *testing.T, factory Factory, worker contractWorker, queueName string) Fixture {
	t.Helper()
	registry := jobq.NewRegistry()
	fixture := factory(t, registry, worker, queueName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = fixture.Stop(ctx)
	})
	return fixture
}

func start(t *testing.T, fixture Fixture) {
	t.Helper()
	if err := fixture.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

// redeliveryLimit bounds waits for redelivery: the job's timeout, the adapter's recovery
// grace, and a fixed margin for scheduling.
func redeliveryLimit(fixture Fixture, timeout time.Duration) time.Duration {
	return timeout + fixture.RecoveryGrace() + 10*time.Second
}

func waitIdle(t *testing.T, fixture Fixture) {
	t.Helper()
	waitIdleWithin(t, fixture, 10*time.Second)
}

func waitIdleWithin(t *testing.T, fixture Fixture, limit time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	if err := fixture.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return receiveWithin(t, ch, 10*time.Second)
}

func receiveWithin[T any](t *testing.T, ch <-chan T, limit time.Duration) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(limit):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func assertNoCall(t *testing.T, calls <-chan contractArgs) {
	t.Helper()
	select {
	case got := <-calls:
		t.Errorf("unexpected Work call: %#v", got)
	default:
	}
}
