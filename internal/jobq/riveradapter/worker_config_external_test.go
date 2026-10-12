package riveradapter_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/worker"
)

func TestWorkerBuildConfigRejectsUnknownQueue(t *testing.T) {
	_, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{Queues: []string{"bogus"}})
	if err == nil {
		t.Fatal("BuildRegistryConfig error = nil, want unknown queue error")
	}
}

func TestWorkerBuildConfigLogsPhysicalQueues(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if _, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{BoundSites: []string{"tokyo"}}); err != nil {
		t.Fatalf("BuildRegistryConfig: %v", err)
	}
	for _, want := range []string{"ingest_tokyo", "epg_tokyo", "reconciler_tokyo", "watcher_tokyo", "cleanup", "ruler", "bound_sites=[tokyo]"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log output does not mention %q; log: %s", want, logBuf.String())
		}
	}
}

func TestWorkerBuildConfigStopAndRescueDefaults(t *testing.T) {
	cfg, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{})
	if err != nil {
		t.Fatalf("BuildRegistryConfig: %v", err)
	}
	if cfg.SoftStopTimeout <= 0 {
		t.Errorf("SoftStopTimeout = %s, want a positive default", cfg.SoftStopTimeout)
	}
	if cfg.RescueStuckJobsAfter != 0 {
		t.Errorf("RescueStuckJobsAfter = %s, want zero to select River default", cfg.RescueStuckJobsAfter)
	}

	explicit, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{
		SoftStopTimeout:      90 * time.Second,
		RescueStuckJobsAfter: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("BuildRegistryConfig with explicit timeouts: %v", err)
	}
	if explicit.SoftStopTimeout != 90*time.Second {
		t.Errorf("explicit SoftStopTimeout = %s, want 90s", explicit.SoftStopTimeout)
	}
	if explicit.RescueStuckJobsAfter != 2*time.Minute {
		t.Errorf("explicit RescueStuckJobsAfter = %s, want 2m", explicit.RescueStuckJobsAfter)
	}
}

func TestWorkerBuildConfigQueueConcurrency(t *testing.T) {
	cfg, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{
		EncodeConcurrency: 3, ThumbnailConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("BuildRegistryConfig: %v", err)
	}
	for queue, want := range map[string]int{"encode": 3, "thumbnail": 2} {
		if got := cfg.Queues[queue].MaxWorkers; got != want {
			t.Errorf("queue %q MaxWorkers = %d, want %d", queue, got, want)
		}
	}
	defaults, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{})
	if err != nil {
		t.Fatalf("BuildRegistryConfig defaults: %v", err)
	}
	for _, queue := range []string{"encode", "thumbnail"} {
		if got := defaults.Queues[queue].MaxWorkers; got != 1 {
			t.Errorf("default queue %q MaxWorkers = %d, want 1", queue, got)
		}
	}
}

func TestWorkerBuildConfigPeriodicJobsAndSiteQueues(t *testing.T) {
	base := riveradapter.Config{
		PeriodicJobs:  true,
		BoundSites:    []string{"tokyo", "takamatsu"},
		CatalogExport: true, DeleteReconcile: true,
		EncodeReconcile: true, ThumbnailReconcile: true,
	}
	configured, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), base)
	if err != nil {
		t.Fatalf("BuildRegistryConfig: %v", err)
	}
	if got, want := len(configured.PeriodicJobs), 14; got != want {
		t.Fatalf("PeriodicJobs = %d, want %d (5 site jobs per site plus 4 global jobs)", got, want)
	}
	for _, name := range []string{"ingest_tokyo", "ingest_takamatsu", "epg_tokyo", "epg_takamatsu", "reconciler_tokyo", "reconciler_takamatsu", "watcher_tokyo", "watcher_takamatsu"} {
		if _, ok := configured.Queues[name]; !ok {
			t.Errorf("physical queue %q missing from River config", name)
		}
	}

	base.PeriodicJobs = false
	disabled, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), base)
	if err != nil {
		t.Fatalf("BuildRegistryConfig with periodic jobs disabled: %v", err)
	}
	if len(disabled.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs with PeriodicJobs=false = %d, want 0", len(disabled.PeriodicJobs))
	}
}

func TestWorkerBuildConfigTSScanPeriodicRegistration(t *testing.T) {
	registered, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{
		PeriodicJobs:    true,
		TSScanReconcile: true,
	})
	if err != nil {
		t.Fatalf("BuildRegistryConfig with periodic TS scan: %v", err)
	}
	if got := len(registered.PeriodicJobs); got != 1 {
		t.Fatalf("PeriodicJobs = %d, want one TS scan reconcile job", got)
	}

	disabled, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{
		PeriodicJobs:    false,
		TSScanReconcile: true,
	})
	if err != nil {
		t.Fatalf("BuildRegistryConfig with periodic jobs disabled: %v", err)
	}
	if got := len(disabled.PeriodicJobs); got != 0 {
		t.Errorf("PeriodicJobs with periodic_jobs=false = %d, want 0", got)
	}
}

func TestWorkerBuildConfigDeleteReconcilePeriodicRegistration(t *testing.T) {
	registered, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{
		PeriodicJobs:    true,
		DeleteReconcile: true,
	})
	if err != nil {
		t.Fatalf("BuildRegistryConfig: %v", err)
	}
	if got := len(registered.PeriodicJobs); got != 1 {
		t.Fatalf("PeriodicJobs = %d, want one delete reconcile job", got)
	}

	disabled, err := riveradapter.BuildRegistryConfig(worker.NewWorkers(&worker.Deps{}), riveradapter.Config{
		PeriodicJobs: true,
	})
	if err != nil {
		t.Fatalf("BuildRegistryConfig with DeleteReconcile disabled: %v", err)
	}
	if got := len(disabled.PeriodicJobs); got != 0 {
		t.Fatalf("PeriodicJobs = %d, want zero when DeleteReconcile is disabled", got)
	}
}
