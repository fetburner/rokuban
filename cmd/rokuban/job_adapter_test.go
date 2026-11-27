package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// TestNewJobAdapterSelectsEngine constructs the same worker and producer adapters as
// server startup. Neither constructor needs a live connection.
func TestNewJobAdapterSelectsEngine(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, tc := range []struct {
		engine config.JobEngine
		counts bool
		grace  time.Duration
	}{
		{engine: "river", counts: true, grace: 5 * time.Minute},
		{engine: "asynq", counts: true, grace: 2*time.Minute + 10*time.Second},
	} {
		t.Run(string(tc.engine), func(t *testing.T) {
			cfg := workerSpecTestConfig(t)
			cfg.Worker.Engine = tc.engine
			cfg.Worker.RescueStuckJobsAfter = nil
			if tc.engine == "river" {
				after := 3 * time.Minute
				cfg.Worker.RescueStuckJobsAfter = &after
			}
			cfg.Worker.RedisURL = "redis://localhost/0"
			cfg.Worker.PeriodicJobs = false
			startup := newWorkerClientConfig(cfg, cfg.Registry(), cfg.Storage.FSStoreNames(), []string{"ruler"}, true, 5*time.Second)
			deps := &worker.Deps{}
			runner, closeRunner, err := newJobAdapter(t.Context(), cfg, &dbHandle{pg: pool}, deps, startup)
			if err != nil {
				t.Fatalf("creating worker adapter: %v", err)
			}
			t.Cleanup(func() { closeJobAdapter(closeRunner) })
			if got := deps.RecoveryGrace; got != tc.grace {
				t.Errorf("worker recovery grace = %s, want %s", got, tc.grace)
			}
			producer, closeProducer, err := newInsertOnlyEnqueuer(t.Context(), cfg, &dbHandle{pg: pool})
			if err != nil {
				t.Fatalf("creating producer adapter: %v", err)
			}
			t.Cleanup(func() { closeJobAdapter(closeProducer) })
			for name, adapter := range map[string]jobq.Enqueuer{"worker": runner, "producer": producer} {
				if _, ok := adapter.(jobq.DeliverableCounter); ok != tc.counts {
					t.Errorf("%s adapter %T exposes deliverable counts = %v, want %v", name, adapter, ok, tc.counts)
				}
			}
		})
	}
}

func TestServerCmd_AsynqRejectsRiverRescueThreshold(t *testing.T) {
	for _, threshold := range []string{"6m", "0s"} {
		t.Run(threshold, func(t *testing.T) {
			path := writeServerTestConfig(t, fmt.Sprintf(`
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://127.0.0.1:1
storage:
  media_dir: /mnt/media
  scratch_dir: ""
worker:
  engine: asynq
  redis_url: redis://localhost/0
  rescue_stuck_jobs_after: %s
`, threshold))
			cmd := newRootCmd()
			cmd.SetArgs([]string{"--config", path, "server", "--roles", "worker"})
			if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "worker.rescue_stuck_jobs_after") {
				t.Fatalf("server startup error = %v, want worker.rescue_stuck_jobs_after rejection", err)
			}
		})
	}
}

func TestMigrateUpSelectsEngineSchema(t *testing.T) {
	for _, engine := range []string{"asynq", "river"} {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			pool := testutil.SetupDB(t)
			// SetupDB clones a migrated DB for this test. Empty its schema to exercise
			// the command's initial migration without changing the shared template.
			testutil.MustExec(t, pool, "resetting isolated test schema", "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
			path := writeWorkerTestConfig(t, "http://127.0.0.1:1", "  engine: "+engine, "  redis_url: redis://localhost/0")
			cmd := newRootCmd()
			cmd.SetArgs([]string{"--config", path, "migrate", "up"})
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			var appTable, riverTables bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass('public.recordings') IS NOT NULL,
				EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND starts_with(tablename, 'river_'))`).Scan(&appTable, &riverTables); err != nil {
				t.Fatal(err)
			}
			if !appTable || riverTables != (engine == "river") {
				t.Errorf("migrate up with %s: recordings table = %v, River tables = %v", engine, appTable, riverTables)
			}
		})
	}
}

type testDeliverableCounter struct{ t *testing.T }

func (c testDeliverableCounter) CountDeliverable(_ context.Context, queue string) (int64, error) {
	if queue != "reconciler_tokyo" {
		c.t.Errorf("queue = %q, want reconciler_tokyo", queue)
	}
	return 7, nil
}

func TestBuildHTTPServer_QueueCountHostExemption(t *testing.T) {
	const countPath = "/internal/queues/reconciler_tokyo/deliverable-count"
	pool, err := pgxpool.New(t.Context(), "postgres://localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, tc := range []struct {
		name        string
		role        string
		counter     jobq.DeliverableCounter
		countStatus int
	}{
		{"asynq notifier", "notifier", testDeliverableCounter{t}, http.StatusOK},
		{"notifier without counter", "notifier", nil, http.StatusBadRequest},
		{"other role", "worker", testDeliverableCounter{t}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workerSpecTestConfig(t)
			cfg.Server.AllowedHosts = []string{"rokuban.example"}
			policy, err := cfg.Storage.BuildStorePolicy()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // The listener handler is under test. Prevent the notifier from dialing Postgres.
			group, ctx := errgroup.WithContext(ctx)
			server, err := buildHTTPServer(ctx, cfg, policy, []string{tc.role}, nil, nil, &dbHandle{pg: pool}, group, true, nil, tc.counter)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := group.Wait(); err != nil {
					t.Error(err)
				}
			})
			for _, req := range []struct {
				method, path, host string
				status             int
			}{
				{http.MethodGet, countPath, "notifier.internal", tc.countStatus},
				{http.MethodGet, countPath + "/extra", "notifier.internal", http.StatusBadRequest},
				{http.MethodPost, countPath, "notifier.internal", http.StatusBadRequest},
				{http.MethodHead, countPath, "notifier.internal", http.StatusBadRequest},
				{http.MethodGet, "/internal//queues/reconciler_tokyo/deliverable-count", "notifier.internal", http.StatusBadRequest},
				{http.MethodGet, "/api/capabilities", "notifier.internal", http.StatusBadRequest},
				{http.MethodGet, "/api/capabilities", "rokuban.example", http.StatusOK},
			} {
				r := httptest.NewRequest(req.method, req.path, nil)
				r.Host = req.host
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, r)
				if w.Code != req.status {
					t.Errorf("%s %s on %s = %d, want %d (body %q)", req.method, req.path, req.host, w.Code, req.status, w.Body)
				}
				if req.path == countPath && req.status == http.StatusOK && w.Body.String() != "{\"value\":7}\n" {
					t.Errorf("count response = %q, want value 7", w.Body)
				}
			}
		})
	}
}
