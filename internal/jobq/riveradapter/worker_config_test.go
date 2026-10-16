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
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/tsscan"
)

func configTestRegistry() *jobq.Registry {
	registry := jobq.NewRegistry()
	Register(registry, riverContractWorkerFunc(func(context.Context, contracttest.Args) error { return nil }))
	return registry
}

// periodicTestRegistry は periodicTestSpec が並べる kind の worker を登録した registry。
func periodicTestRegistry() *jobq.Registry {
	registry := configTestRegistry()
	Register(registry, nopWorker[jobs.EpgSyncArgs]{})
	Register(registry, nopWorker[jobs.TunerSyncArgs]{})
	Register(registry, nopWorker[jobs.RulerPassArgs]{})
	Register(registry, nopWorker[jobs.ReconcilePassArgs]{})
	Register(registry, nopWorker[jobs.RecordSweepArgs]{})
	Register(registry, nopWorker[jobs.CatalogExportArgs]{})
	Register(registry, nopWorker[jobs.DeleteReconcileArgs]{})
	Register(registry, nopWorker[jobs.EncodeReconcileArgs]{})
	Register(registry, nopWorker[tsscan.ReconcileArgs]{})
	return registry
}

// periodicTestSpec は sites × 5 種の site 単位ジョブと、site 非依存 4 種を並べた仕様。
func periodicTestSpec(sites ...string) jobq.WorkerSpec {
	spec := siteSpec(1, sites...)
	for _, site := range sites {
		spec.PeriodicJobs = append(spec.PeriodicJobs,
			jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.EpgSyncArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.TunerSyncArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.RulerPassArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.ReconcilePassArgs{Site: site}},
			jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.RecordSweepArgs{Site: site}},
		)
	}
	spec.PeriodicJobs = append(spec.PeriodicJobs,
		jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.CatalogExportArgs{}},
		jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.DeleteReconcileArgs{}},
		jobq.PeriodicJobSpec{Interval: oneHour, Args: jobs.EncodeReconcileArgs{}},
		jobq.PeriodicJobSpec{Interval: oneHour, Args: tsscan.ReconcileArgs{}},
	)
	return spec
}

func TestWorkerBuildConfigRejectsUnknownQueue(t *testing.T) {
	_, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: siteSpec(1, "tokyo"), Queues: []string{"bogus"}})
	if err == nil || !strings.Contains(err.Error(), `worker.queues: unknown queue "bogus"`) {
		t.Fatalf("buildRegistryConfig error = %v, want unknown queue error", err)
	}
}

// 仕様が不正なら起動時に拒む（Validate の各条件は jobq の TestWorkerSpecValidate が見る。
// ここは adapter が Validate を呼ぶことだけを固定する）。
func TestWorkerBuildConfigRejectsInvalidSpec(t *testing.T) {
	spec := siteSpec(1, "tokyo")
	spec.Queues[0].MaxWorkers = 0
	_, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "MaxWorkers must be >= 1") {
		t.Fatalf("buildRegistryConfig error = %v, want MaxWorkers error", err)
	}
}

// 定期ジョブの kind が registry で解決できなければ、SchedulePeriodicJobs の値に関わらず拒む。
func TestWorkerBuildConfigRejectsPeriodicKindNotInRegistry(t *testing.T) {
	spec := siteSpec(1, "tokyo")
	spec.PeriodicJobs = []jobq.PeriodicJobSpec{{Interval: oneHour, Args: jobs.DeleteReconcileArgs{}}}
	for _, schedule := range []bool{true, false} {
		_, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec, SchedulePeriodicJobs: schedule})
		if err == nil || !strings.Contains(err.Error(), `"delete_reconcile"`) {
			t.Fatalf("SchedulePeriodicJobs=%v: error = %v, want unresolvable delete_reconcile", schedule, err)
		}
	}
	if _, err := buildRegistryConfig(periodicTestRegistry(), testQueueFor, Config{Spec: spec}); err != nil {
		t.Errorf("registered kind: %v", err)
	}
}

func TestWorkerBuildConfigLogsPhysicalQueues(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if _, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: siteSpec(1, "tokyo")}); err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	for _, want := range []string{"ingest_tokyo", "epg_tokyo", "reconciler_tokyo", "watcher_tokyo", "cleanup", "ruler"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log output does not mention %q; log: %s", want, logBuf.String())
		}
	}
}

func TestWorkerBuildConfigStopAndRescueDefaults(t *testing.T) {
	spec := siteSpec(1, "tokyo")
	cfg, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	if cfg.SoftStopTimeout <= 0 {
		t.Errorf("SoftStopTimeout = %s, want a positive default", cfg.SoftStopTimeout)
	}
	if cfg.RescueStuckJobsAfter != 0 {
		t.Errorf("RescueStuckJobsAfter = %s, want zero to select River default", cfg.RescueStuckJobsAfter)
	}

	explicit, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{
		Spec:                 spec,
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

// 渡した仕様の並列数が、物理キューごとにそのまま River の MaxWorkers になる。
// 値は仕様に与えたリテラルで、adapter 側に既定値は無い。
func TestWorkerBuildConfigQueueConcurrencyFollowsSpec(t *testing.T) {
	spec := siteSpec(4, "tokyo", "takamatsu")
	spec.Queues = append(spec.Queues, plainQueue("extra", 7))
	cfg, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	for queue, want := range map[string]int{
		"ingest_tokyo": 4, "ingest_takamatsu": 4,
		"encode": 1, "thumbnail": 1, "default": 100, "cleanup": 2, "extra": 7,
	} {
		if got := cfg.Queues[queue].MaxWorkers; got != want {
			t.Errorf("queue %q MaxWorkers = %d, want %d", queue, got, want)
		}
	}
}

// 物理キューは仕様の Physical をそのまま購読する（site 単位 4 種 × 2 サイト）。
func TestWorkerBuildConfigSubscribesEveryPhysicalQueueInSpec(t *testing.T) {
	cfg, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: siteSpec(1, "tokyo", "takamatsu")})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	for _, name := range []string{"ingest_tokyo", "ingest_takamatsu", "epg_tokyo", "epg_takamatsu", "reconciler_tokyo", "reconciler_takamatsu", "watcher_tokyo", "watcher_takamatsu"} {
		if _, ok := cfg.Queues[name]; !ok {
			t.Errorf("physical queue %q missing from River config", name)
		}
	}
	for _, name := range []string{"ingest", "epg", "reconciler", "watcher", "ingest_default"} {
		if _, ok := cfg.Queues[name]; ok {
			t.Errorf("unexpected physical queue %q in River config", name)
		}
	}
	if got, want := len(cfg.Queues), 8+6; got != want {
		t.Errorf("len(Queues) = %d, want %d", got, want)
	}
}

// 選んだキューの Physical が 0 件（site で分けるキューを 0 サイトで選んだ）なら起動を拒む。
// 選ばなければ（site 非依存のキューだけなら）通る --- 両方向。
func TestWorkerBuildConfigRejectsSelectedQueueWithoutPhysicalNames(t *testing.T) {
	spec := siteSpec(1) // 0 サイト
	_, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "no physical queue") {
		t.Fatalf("all queues with 0 sites: error = %v, want no physical queue", err)
	}
	_, err = buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec, Queues: []string{jobs.IngestQueue}})
	if err == nil || !strings.Contains(err.Error(), `"ingest"`) {
		t.Fatalf("ingest with 0 sites: error = %v, want ingest named", err)
	}
	if _, err := buildRegistryConfig(configTestRegistry(), testQueueFor, Config{Spec: spec, Queues: []string{jobs.RulerQueue, jobs.CleanupQueue}}); err != nil {
		t.Errorf("site-independent queues with 0 sites: %v", err)
	}
}

// 登録する定期ジョブの数は、渡した仕様の数（2 サイト × 5 種 + 4 種）に一致し、
// SchedulePeriodicJobs=false なら 0 になる（仕様の一覧は空にならない）。
func TestWorkerBuildConfigPeriodicJobsFollowSpec(t *testing.T) {
	spec := periodicTestSpec("tokyo", "takamatsu")
	if got, want := len(spec.PeriodicJobs), 14; got != want {
		t.Fatalf("fixture has %d periodic jobs, want %d", got, want)
	}
	configured, err := buildRegistryConfig(periodicTestRegistry(), testQueueFor, Config{Spec: spec, SchedulePeriodicJobs: true})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	if got, want := len(configured.PeriodicJobs), 14; got != want {
		t.Errorf("PeriodicJobs = %d, want %d", got, want)
	}

	disabled, err := buildRegistryConfig(periodicTestRegistry(), testQueueFor, Config{Spec: spec, SchedulePeriodicJobs: false})
	if err != nil {
		t.Fatalf("buildRegistryConfig with SchedulePeriodicJobs=false: %v", err)
	}
	if got := len(disabled.PeriodicJobs); got != 0 {
		t.Errorf("PeriodicJobs with SchedulePeriodicJobs=false = %d, want 0", got)
	}
}

// 定期ジョブは --queues の絞り込みに従わない（購読の絞り込みと投入の契機は別の軸）。
func TestWorkerBuildConfigPeriodicJobsIgnoreQueueSelection(t *testing.T) {
	spec := periodicTestSpec("tokyo")
	configured, err := buildRegistryConfig(periodicTestRegistry(), testQueueFor, Config{
		Spec: spec, Queues: []string{jobs.TSScanQueue}, SchedulePeriodicJobs: true,
	})
	if err != nil {
		t.Fatalf("buildRegistryConfig: %v", err)
	}
	if got, want := len(configured.PeriodicJobs), 9; got != want {
		t.Errorf("PeriodicJobs = %d, want %d", got, want)
	}
	if got := len(configured.Queues); got != 1 {
		t.Errorf("Queues = %d, want 1 (ts_scan only)", got)
	}
}

// 定期ジョブの投入先（queueFor）が、仕様の購読する物理名に入らなければ起動を拒む。
// 通る queueFor なら起動できる --- 両方向。
func TestWorkerBuildConfigRejectsQueueForDisagreeingWithSpec(t *testing.T) {
	spec := periodicTestSpec("tokyo")
	if _, err := buildRegistryConfig(periodicTestRegistry(), testQueueFor, Config{Spec: spec}); err != nil {
		t.Fatalf("agreeing queueFor: %v", err)
	}
	_, err := buildRegistryConfig(periodicTestRegistry(), logicalQueueFor, Config{Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "nobody would pull it") {
		t.Fatalf("logical-name queueFor: error = %v, want a mismatch error", err)
	}
}
