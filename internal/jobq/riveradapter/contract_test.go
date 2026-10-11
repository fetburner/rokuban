package riveradapter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
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

type riverContractFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	registry *jobq.Registry
	cfg      Config
	current  *Adapter
	clients  []*Adapter
}

func newRiverContractFixture(t *testing.T, registry *jobq.Registry, worker jobq.Worker[contracttest.Args]) contracttest.Fixture {
	jobqAdapter := &riverContractFixture{
		t:        t,
		pool:     testutil.SetupDB(t),
		registry: registry,
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
	client, err := NewAdapter(fixture.pool, fixture.registry, fixture.cfg)
	if err != nil {
		fixture.t.Fatalf("NewAdapter() error = %v", err)
	}
	fixture.clients = append(fixture.clients, client)
	return client
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
