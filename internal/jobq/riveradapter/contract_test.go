package riveradapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

const riverContractRescueAfter = 6 * time.Second

func TestMapWorkerError(t *testing.T) {
	t.Run("snooze maps to River snooze", func(t *testing.T) {
		got := mapWorkerError(jobq.Snooze(250 * time.Millisecond))
		var snooze *river.JobSnoozeError
		if !errors.As(got, &snooze) {
			t.Fatalf("mapped error = %T (%v), want *river.JobSnoozeError", got, got)
		}
		if snooze.Duration != 250*time.Millisecond {
			t.Errorf("snooze duration = %s, want 250ms", snooze.Duration)
		}
	})

	t.Run("discard maps to River cancellation", func(t *testing.T) {
		got := mapWorkerError(jobq.ErrDiscard)
		var cancel *river.JobCancelError
		if !errors.As(got, &cancel) {
			t.Fatalf("mapped error = %T (%v), want *river.JobCancelError", got, got)
		}
		if !errors.Is(got, jobq.ErrDiscard) {
			t.Errorf("mapped error = %v, want to retain jobq.ErrDiscard", got)
		}
	})

	t.Run("ordinary failure is preserved", func(t *testing.T) {
		want := errors.New("retryable")
		if got := mapWorkerError(want); !errors.Is(got, want) {
			t.Errorf("mapped error = %v, want the original error", got)
		}
	})
}

func TestContract(t *testing.T) {
	contracttest.Run(t, newRiverContractFixture)
}

func TestStopAllowsActiveWorkToEnqueue(t *testing.T) {
	var adapter *Adapter
	started := make(chan struct{}, 1)
	enqueued := make(chan error, 1)
	worker := riverContractWorkerFunc(func(ctx context.Context, args contracttest.Args) error {
		if args.Value != "active" {
			return nil
		}
		started <- struct{}{}
		<-ctx.Done()
		_, err := adapter.Enqueue(context.Background(), contracttest.Args{
			Value: "enqueued during stop", TimeoutMS: 1000, MaxAttempts: 1,
		})
		enqueued <- err
		return err
	})
	adapter = newLifecycleTestAdapter(t, worker, 20*time.Millisecond)
	startAdapter(t, adapter)
	if _, err := adapter.Enqueue(context.Background(), contracttest.Args{
		Value: "active", TimeoutMS: 5000, MaxAttempts: 1,
	}); err != nil {
		t.Fatalf("Enqueue(active) error = %v", err)
	}
	receiveSignal(t, started)

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := adapter.Stop(stopCtx)
	cancel()
	if err != nil {
		t.Fatalf("Stop() error = %v, want nil after cancelled Work enqueues", err)
	}
	if err := receiveError(t, enqueued); err != nil {
		t.Fatalf("Enqueue() from Work error = %v", err)
	}
}

func TestStartAfterSuccessfulStopUsesNewClient(t *testing.T) {
	adapter := newLifecycleTestAdapter(t, riverContractWorkerFunc(func(context.Context, contracttest.Args) error { return nil }), time.Second)
	startAdapter(t, adapter)
	first := adapter.clientSnapshot()
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := adapter.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	startAdapter(t, adapter)
	if adapter.clientSnapshot() == first {
		t.Fatal("Start() after a successful Stop reused the stopped River client")
	}
}

func TestStartAfterStopBeforeStartDoesNotBlock(t *testing.T) {
	adapter := newLifecycleTestAdapter(t, riverContractWorkerFunc(func(context.Context, contracttest.Args) error { return nil }), time.Second)
	if err := adapter.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() before Start error = %v", err)
	}
	startCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adapter.Start(startCtx); err != nil {
		t.Fatalf("Start() after Stop before any Start error = %v", err)
	}
}

func TestRestartAfterTimedOutStopAllowsActiveWorkToEnqueue(t *testing.T) {
	var adapter *Adapter
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	releaseWork := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWork) }) }
	defer release()
	enqueued := make(chan error, 1)
	followupRan := make(chan struct{}, 1)
	worker := riverContractWorkerFunc(func(ctx context.Context, args contracttest.Args) error {
		if args.Value != "active" {
			followupRan <- struct{}{}
			return nil
		}
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-releaseWork
		_, err := adapter.Enqueue(context.Background(), contracttest.Args{
			Value: "follow-up", TimeoutMS: 1000, MaxAttempts: 1,
		})
		enqueued <- err
		return err
	})
	adapter = newLifecycleTestAdapter(t, worker, 20*time.Millisecond)
	startAdapter(t, adapter)
	if _, err := adapter.Enqueue(context.Background(), contracttest.Args{
		Value: "active", TimeoutMS: 5000, MaxAttempts: 1,
	}); err != nil {
		t.Fatalf("Enqueue(active) error = %v", err)
	}
	receiveSignal(t, started)

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := adapter.Stop(stopCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want DeadlineExceeded while Work is held", err)
	}
	receiveSignal(t, cancelled)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStart()
	startCtxObserved := &signalDoneContext{Context: startCtx, doneObserved: make(chan struct{})}
	startedAgain := make(chan error, 1)
	go func() { startedAgain <- adapter.Start(startCtxObserved) }()
	receiveSignal(t, startCtxObserved.doneObserved)
	release()
	if err := receiveError(t, enqueued); err != nil {
		t.Fatalf("Enqueue() from Work error = %v", err)
	}
	if err := receiveError(t, startedAgain); err != nil {
		t.Fatalf("Start() after timed-out Stop error = %v", err)
	}
	receiveSignal(t, followupRan)
}

type riverContractFixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	registry  *jobq.Registry
	cfg       Config
	queueName string
	current   *Adapter
	clients   []*Adapter
}

func newRiverContractFixture(t *testing.T, registry *jobq.Registry, worker jobq.Worker[contracttest.Args], queueName string) contracttest.Fixture {
	jobqAdapter := &riverContractFixture{
		t:         t,
		pool:      testutil.SetupDB(t),
		registry:  registry,
		queueName: queueName,
		cfg: Config{
			Queues:               []string{"default"},
			JobTimeout:           time.Millisecond,
			RescueStuckJobsAfter: riverContractRescueAfter,
			TestOnly:             true,
		},
	}
	Register(registry, worker)
	jobqAdapter.current = jobqAdapter.newClient()
	return jobqAdapter
}

func (fixture *riverContractFixture) newClient() *Adapter {
	fixture.t.Helper()
	riverCfg, err := buildRegistryConfig(fixture.registry, fixture.cfg)
	if err != nil {
		fixture.t.Fatalf("buildRegistryConfig() error = %v", err)
	}
	if _, exists := riverCfg.Queues[fixture.queueName]; exists {
		// Use the production constructor whenever it already processes the queue under test,
		// so contract cases notice changes to NewAdapter.
		client, err := NewAdapter(fixture.pool, fixture.registry, fixture.cfg)
		if err != nil {
			fixture.t.Fatalf("NewAdapter() error = %v", err)
		}
		fixture.clients = append(fixture.clients, client)
		return client
	}
	// The contract passes a physical queue name that is not an application logical queue, which
	// NewAdapter never processes. Inject it through River's public config so the fixture
	// actually delivers jobs from that name.
	riverCfg.Queues[fixture.queueName] = river.QueueConfig{MaxWorkers: 1}
	client, err := river.NewClient(riverpgxv5.New(fixture.pool), riverCfg)
	if err != nil {
		fixture.t.Fatalf("river.NewClient() error = %v", err)
	}
	adapter := &Adapter{pool: fixture.pool, registry: fixture.registry, config: fixture.cfg, client: client}
	fixture.clients = append(fixture.clients, adapter)
	return adapter
}

func (fixture *riverContractFixture) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	return fixture.current.Enqueue(ctx, args, opts...)
}

func (fixture *riverContractFixture) EnqueueTx(ctx context.Context, tx db.Tx, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	return fixture.current.EnqueueTx(ctx, tx, args, opts...)
}

func (fixture *riverContractFixture) Start(ctx context.Context) error {
	if fixture.current == nil {
		fixture.current = fixture.newClient()
	}
	return fixture.current.Start(ctx)
}

func (fixture *riverContractFixture) Stop(ctx context.Context) error {
	var errs []error
	for _, client := range fixture.clients {
		if err := client.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (fixture *riverContractFixture) WaitIdle(ctx context.Context) error {
	if fixture.current == nil {
		return errors.New("river contract fixture has no client")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := fixture.current.client.JobList(ctx, river.NewJobListParams().
			Kinds(contracttest.Args{}.Kind()).
			States(
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			))
		if err != nil {
			return fmt.Errorf("listing active contract jobs: %w", err)
		}
		if len(result.Jobs) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (fixture *riverContractFixture) BeginTx(ctx context.Context) (db.Tx, error) {
	return fixture.pool.BeginTx(ctx, pgx.TxOptions{})
}

func (fixture *riverContractFixture) Crash(ctx context.Context) error {
	if fixture.current == nil || fixture.current.client == nil {
		return errors.New("river contract fixture has no client to crash")
	}
	running, err := fixture.current.client.JobList(ctx, river.NewJobListParams().
		Kinds(contracttest.Args{}.Kind()).
		States(rivertype.JobStateRunning))
	if err != nil {
		return fmt.Errorf("finding running contract job: %w", err)
	}
	if len(running.Jobs) != 1 {
		return fmt.Errorf("running contract jobs = %d, want 1", len(running.Jobs))
	}
	jobID := running.Jobs[0].ID
	// Let the active Work observe cancellation and finish so the claimant relinquishes
	// leadership. The next step recreates the running row a crashed process leaves behind.
	stopCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	stopErr := fixture.current.client.StopAndCancel(stopCtx)
	cancel()
	if stopErr != nil {
		return fmt.Errorf("stopping crashed client: %w", stopErr)
	}
	// Recreate the persisted state left by a process that died during Work. River may
	// settle the cancelled job into a different state during shutdown, so replace that
	// state directly after the claimant has released leadership.
	staleAt := time.Now().Add(-riverContractRescueAfter - time.Second)
	command, err := fixture.pool.Exec(ctx,
		//nolint:gocritic // Invariant 14 permits River SQL only to create stale rescue state in this test.
		"UPDATE river_job SET state = 'running', attempted_at = $1 WHERE id = $2",
		staleAt, jobID)
	if err != nil {
		return fmt.Errorf("creating stale running state for rescue contract: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("running rescue rows = %d, want 1", command.RowsAffected())
	}

	resident := fixture.newClient()
	if err := resident.Start(context.Background()); err != nil {
		return fmt.Errorf("starting resident River client: %w", err)
	}
	fixture.current = resident
	return nil
}

func (fixture *riverContractFixture) RecoveryGrace() time.Duration {
	// TestContract's process-death case measures recovery through the configured River rescuer.
	return riverContractRescueAfter + 2*time.Minute
}

var _ contracttest.Fixture = (*riverContractFixture)(nil)

type riverContractWorkerFunc func(context.Context, contracttest.Args) error

func (worker riverContractWorkerFunc) Work(ctx context.Context, args contracttest.Args) error {
	return worker(ctx, args)
}

func newLifecycleTestAdapter(t *testing.T, worker jobq.Worker[contracttest.Args], softStopTimeout time.Duration) *Adapter {
	t.Helper()
	registry := jobq.NewRegistry()
	Register(registry, worker)
	adapter, err := NewAdapter(testutil.SetupDB(t), registry, Config{
		Queues:               []string{"default"},
		JobTimeout:           time.Millisecond,
		RescueStuckJobsAfter: riverContractRescueAfter,
		SoftStopTimeout:      softStopTimeout,
		TestOnly:             true,
	})
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = adapter.Stop(ctx)
	})
	return adapter
}

func startAdapter(t *testing.T, adapter *Adapter) {
	t.Helper()
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

func receiveSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func receiveError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
		return nil
	}
}

type signalDoneContext struct {
	context.Context
	doneObserved chan struct{}
	once         sync.Once
}

func (ctx *signalDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.doneObserved) })
	return ctx.Context.Done()
}
