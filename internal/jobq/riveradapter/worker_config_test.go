package riveradapter

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
)

func configTestRegistry() *jobq.Registry {
	registry := jobq.NewRegistry()
	Register(registry, riverContractWorkerFunc(func(context.Context, contracttest.Args) error { return nil }))
	return registry
}

func TestWorkerBuildConfigRejectsUnknownQueue(t *testing.T) {
	_, err := buildRegistryConfig(configTestRegistry(), Config{Queues: []string{"bogus"}})
	if err == nil {
		t.Fatal("buildRegistryConfig error = nil, want unknown queue error")
	}
}

func TestWorkerBuildConfigLogsPhysicalQueues(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if _, err := buildRegistryConfig(configTestRegistry(), Config{BoundSites: []string{"tokyo"}}); err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	for _, want := range []string{"ingest_tokyo", "epg_tokyo", "reconciler_tokyo", "watcher_tokyo", "cleanup", "ruler", "bound_sites=[tokyo]"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log output does not mention %q; log: %s", want, logBuf.String())
		}
	}
}

func TestWorkerBuildConfigStopAndRescueDefaults(t *testing.T) {
	cfg, err := buildRegistryConfig(configTestRegistry(), Config{})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	if cfg.SoftStopTimeout <= 0 {
		t.Errorf("SoftStopTimeout = %s, want a positive default", cfg.SoftStopTimeout)
	}
	if cfg.RescueStuckJobsAfter != 0 {
		t.Errorf("RescueStuckJobsAfter = %s, want zero to select River default", cfg.RescueStuckJobsAfter)
	}

	explicit, err := buildRegistryConfig(configTestRegistry(), Config{
		SoftStopTimeout:      90 * time.Second,
		RescueStuckJobsAfter: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig with explicit timeouts: %v", err)
	}
	if explicit.SoftStopTimeout != 90*time.Second {
		t.Errorf("explicit SoftStopTimeout = %s, want 90s", explicit.SoftStopTimeout)
	}
	if explicit.RescueStuckJobsAfter != 2*time.Minute {
		t.Errorf("explicit RescueStuckJobsAfter = %s, want 2m", explicit.RescueStuckJobsAfter)
	}
}

func TestWorkerBuildConfigQueueConcurrency(t *testing.T) {
	cfg, err := buildRegistryConfig(configTestRegistry(), Config{
		EncodeConcurrency: 3, ThumbnailConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	for queue, want := range map[string]int{"encode": 3, "thumbnail": 2} {
		if got := cfg.Queues[queue].MaxWorkers; got != want {
			t.Errorf("queue %q MaxWorkers = %d, want %d", queue, got, want)
		}
	}
	defaults, err := buildRegistryConfig(configTestRegistry(), Config{})
	if err != nil {
		t.Fatalf("buildRegistryConfig defaults: %v", err)
	}
	for _, queue := range []string{"encode", "thumbnail"} {
		if got := defaults.Queues[queue].MaxWorkers; got != 1 {
			t.Errorf("default queue %q MaxWorkers = %d, want 1", queue, got)
		}
	}
}

func TestWorkerBuildConfigPeriodicJobsAndSiteQueues(t *testing.T) {
	base := Config{
		PeriodicJobs:  true,
		BoundSites:    []string{"tokyo", "takamatsu"},
		CatalogExport: true, DeleteReconcile: true,
		EncodeReconcile: true, ThumbnailReconcile: true,
	}
	configured, err := buildRegistryConfig(configTestRegistry(), base)
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
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
	disabled, err := buildRegistryConfig(configTestRegistry(), base)
	if err != nil {
		t.Fatalf("buildRegistryConfig with periodic jobs disabled: %v", err)
	}
	if len(disabled.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs with PeriodicJobs=false = %d, want 0", len(disabled.PeriodicJobs))
	}
}

func TestWorkerBuildConfigTSScanPeriodicRegistration(t *testing.T) {
	registered, err := buildRegistryConfig(configTestRegistry(), Config{
		PeriodicJobs:    true,
		TSScanReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig with periodic TS scan: %v", err)
	}
	if got := len(registered.PeriodicJobs); got != 1 {
		t.Fatalf("PeriodicJobs = %d, want one TS scan reconcile job", got)
	}

	disabled, err := buildRegistryConfig(configTestRegistry(), Config{
		PeriodicJobs:    false,
		TSScanReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig with periodic jobs disabled: %v", err)
	}
	if got := len(disabled.PeriodicJobs); got != 0 {
		t.Errorf("PeriodicJobs with periodic_jobs=false = %d, want 0", got)
	}
}

func TestWorkerBuildConfigDeleteReconcilePeriodicRegistration(t *testing.T) {
	registered, err := buildRegistryConfig(configTestRegistry(), Config{
		PeriodicJobs:    true,
		DeleteReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	if got := len(registered.PeriodicJobs); got != 1 {
		t.Fatalf("PeriodicJobs = %d, want one delete reconcile job", got)
	}

	disabled, err := buildRegistryConfig(configTestRegistry(), Config{
		PeriodicJobs: true,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig with DeleteReconcile disabled: %v", err)
	}
	if got := len(disabled.PeriodicJobs); got != 0 {
		t.Fatalf("PeriodicJobs = %d, want zero when DeleteReconcile is disabled", got)
	}
}
