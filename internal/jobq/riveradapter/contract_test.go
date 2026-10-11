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
	t                       *testing.T
	pool                    *pgxpool.Pool
	registry                *jobq.Registry
	cfg                     Config
	current                 *Adapter
	clients                 []*Adapter
	claimantApplicationName string
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
	jobqAdapter.current = jobqAdapter.newClient("claimant")
	return jobqAdapter
}

func (fixture *riverContractFixture) newClient(role string) *Adapter {
	fixture.t.Helper()
	poolConfig, err := pgxpool.ParseConfig(testutil.DatabaseConfig(fixture.t).DSN())
	if err != nil {
		fixture.t.Fatalf("parsing River client pool config: %v", err)
	}
	applicationName := "rokuban-riveradapter-" + role
	poolConfig.ConnConfig.RuntimeParams["application_name"] = applicationName
	clientPool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		fixture.t.Fatalf("creating River client pool: %v", err)
	}
	fixture.t.Cleanup(clientPool.Close)
	client, err := NewAdapter(clientPool, fixture.registry, fixture.cfg)
	if err != nil {
		fixture.t.Fatalf("NewAdapter() error = %v", err)
	}
	if role == "claimant" {
		fixture.claimantApplicationName = applicationName
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
		fixture.current = fixture.newClient("claimant")
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
	resident := fixture.newClient("resident")
	if err := resident.Start(ctx); err != nil {
		return fmt.Errorf("starting resident River client: %w", err)
	}

	// Stop the claimant but leave its in-flight Work blocked. A separate client becomes the
	// resident leader and must recover the stale running row through River's rescuer.
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	stopErr := fixture.current.client.StopAndCancel(stopCtx)
	cancel()
	if stopErr != nil && !errors.Is(stopErr, context.DeadlineExceeded) {
		return fmt.Errorf("stopping crashed client: %w", stopErr)
	}

	staleAt := time.Now().Add(-riverContractRescueAfter - time.Second)
	command, err := fixture.pool.Exec(ctx,
		//nolint:gocritic // Invariant 14 permits River SQL only to age the row in this rescue test.
		"UPDATE river_job SET attempted_at = $1 WHERE id = $2 AND state = 'running'",
		staleAt, jobID)
	if err != nil {
		return fmt.Errorf("marking running job stale for rescue contract: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("stale running rows = %d, want 1", command.RowsAffected())
	}
	// A timed-out StopAndCancel cannot release the claimant while its Work is deliberately
	// blocked. Terminate only this fixture's PostgreSQL sessions to model the process exiting;
	// the resident client then has to acquire leadership and rescue the still-running job.
	if err := fixture.terminateClientConnections(ctx, fixture.claimantApplicationName); err != nil {
		return fmt.Errorf("terminating crashed client's PostgreSQL sessions: %w", err)
	}

	fixture.current = resident
	return nil
}

func (fixture *riverContractFixture) terminateClientConnections(ctx context.Context, applicationName string) error {
	rows, err := fixture.pool.Query(ctx,
		"SELECT pid FROM pg_stat_activity WHERE application_name = $1 AND pid <> pg_backend_pid()",
		applicationName)
	if err != nil {
		return fmt.Errorf("listing client PostgreSQL sessions: %w", err)
	}
	var pids []int32
	for rows.Next() {
		var pid int32
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return fmt.Errorf("scanning client PostgreSQL session: %w", err)
		}
		pids = append(pids, pid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading client PostgreSQL sessions: %w", err)
	}
	rows.Close()
	if len(pids) == 0 {
		return fmt.Errorf("no PostgreSQL sessions found for application_name %q", applicationName)
	}
	for _, pid := range pids {
		var terminated bool
		if err := fixture.pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil {
			return fmt.Errorf("terminating PostgreSQL backend %d: %w", pid, err)
		}
		if !terminated {
			return fmt.Errorf("PostgreSQL backend %d was not terminated", pid)
		}
	}
	return nil
}

func (fixture *riverContractFixture) RecoveryGrace() time.Duration {
	// TestContract's process-death case measures recovery through the configured River rescuer.
	return riverContractRescueAfter + 2*time.Minute
}

var _ contracttest.Fixture = (*riverContractFixture)(nil)
