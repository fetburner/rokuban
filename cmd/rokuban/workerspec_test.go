package main

import (
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/worker"
)

// workerSpecTestConfig は 2 サイトの最小 config を読む。並列数と epg 間隔は、
// 既定値と取り違えないよう既定値と異なる値を与える。
func workerSpecTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
storage:
  media_dir: /mnt/media
ingest:
  concurrency: 5
encode:
  concurrency: 2
  thumbnail_concurrency: 3
epg:
  sync_interval: 7m
`))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

type periodicWant struct {
	kind     string
	site     string
	interval time.Duration
	queue    string
}

func periodicGot(spec jobq.WorkerSpec) []periodicWant {
	got := make([]periodicWant, 0, len(spec.PeriodicJobs))
	for _, p := range spec.PeriodicJobs {
		site := ""
		if b, ok := p.Args.(interface{ QueueSite() string }); ok {
			site = b.QueueSite()
		}
		if r, ok := p.Args.(jobs.RulerPassArgs); ok {
			site = r.Site
		}
		got = append(got, periodicWant{p.Args.Kind(), site, p.Interval, p.Args.Spec().Queue})
	}
	return got
}

// 製品の仕様（並列数・物理キュー）を、実装の定数ではなくリテラルで固定する。
func TestNewWorkerSpec_Queues(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	spec := newWorkerSpec(cfg, cfg.Registry())
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []jobq.QueueSpec{
		{Logical: "default", Physical: []string{"default"}, MaxWorkers: 100},
		{Logical: "ingest", Physical: []string{"ingest_tokyo", "ingest_takamatsu"}, MaxWorkers: 5},
		{Logical: "epg", Physical: []string{"epg_tokyo", "epg_takamatsu"}, MaxWorkers: 1},
		{Logical: "ruler", Physical: []string{"ruler"}, MaxWorkers: 1},
		{Logical: "reconciler", Physical: []string{"reconciler_tokyo", "reconciler_takamatsu"}, MaxWorkers: 1},
		{Logical: "watcher", Physical: []string{"watcher_tokyo", "watcher_takamatsu"}, MaxWorkers: 1},
		{Logical: "encode", Physical: []string{"encode"}, MaxWorkers: 2},
		{Logical: "thumbnail", Physical: []string{"thumbnail"}, MaxWorkers: 3},
		{Logical: "cm_detect", Physical: []string{"cm_detect"}, MaxWorkers: 1},
		{Logical: "cleanup", Physical: []string{"cleanup"}, MaxWorkers: 2},
		{Logical: "storage", Physical: []string{"storage"}, MaxWorkers: 1},
		{Logical: "transfer", Physical: []string{"transfer_test-store"}, MaxWorkers: 1},
		{Logical: "ts_scan", Physical: []string{"ts_scan"}, MaxWorkers: 1},
	}
	if len(spec.Queues) != len(want) {
		t.Fatalf("len(Queues) = %d, want %d: %+v", len(spec.Queues), len(want), spec.Queues)
	}
	for i, w := range want {
		g := spec.Queues[i]
		if g.Logical != w.Logical || g.MaxWorkers != w.MaxWorkers || !slices.Equal(g.Physical, w.Physical) {
			t.Errorf("Queues[%d] = %+v, want %+v", i, g, w)
		}
	}
}

// 論理キューの一覧は jobs.AllQueueNames（--queues の許可リストと k8s の網）と一致する。
func TestNewWorkerSpec_QueuesMatchAllQueueNames(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	var got []string
	for _, q := range newWorkerSpec(cfg, cfg.Registry()).Queues {
		got = append(got, q.Logical)
	}
	slices.Sort(got)
	if want := jobs.AllQueueNames(); !slices.Equal(got, want) {
		t.Errorf("logical queues = %v, want jobs.AllQueueNames() = %v", got, want)
	}
}

func TestNewWorkerSpec_PeriodicJobs(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	got := periodicGot(newWorkerSpec(cfg, cfg.Registry()))
	want := []periodicWant{
		{"epg_sync", "tokyo", 7 * time.Minute, "epg"},
		{"tuner_sync", "tokyo", 10 * time.Minute, "epg"},
		{"ruler_pass", "tokyo", 10 * time.Minute, "ruler"},
		{"reconcile_pass", "tokyo", 30 * time.Second, "reconciler"},
		{"record_sweep", "tokyo", 5 * time.Minute, "watcher"},
		{"epg_sync", "takamatsu", 7 * time.Minute, "epg"},
		{"tuner_sync", "takamatsu", 10 * time.Minute, "epg"},
		{"ruler_pass", "takamatsu", 10 * time.Minute, "ruler"},
		{"reconcile_pass", "takamatsu", 30 * time.Second, "reconciler"},
		{"record_sweep", "takamatsu", 5 * time.Minute, "watcher"},
		{"catalog_export", "", 24 * time.Hour, "cleanup"},
		{"delete_reconcile", "", 15 * time.Minute, "cleanup"},
		{"label_rule_reconcile", "", 15 * time.Minute, "cleanup"},
		{"encode_reconcile", "", 15 * time.Minute, "encode"},
		{"thumbnail_reconcile", "", 15 * time.Minute, "thumbnail"},
		{"ts_scan_reconcile", "", 15 * time.Minute, "ts_scan"},
		{"storage_sync", "", 5 * time.Minute, "storage"},
		{"storage_transfer_reconcile", "", 15 * time.Minute, "storage"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("periodic jobs =\n%v\nwant\n%v", got, want)
	}
}

func TestWorkerQueueSpecsQualifyTransferByMountedFSStore(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.Storage.Stores = map[string]config.StoreConfig{
		"tokyo":     {Kind: "fs"},
		"takamatsu": {Kind: "fs"},
		"s3main":    {Kind: "s3"},
	}
	cfg.Storage.MediaStoreName = "tokyo"
	for _, spec := range workerQueueSpecs(cfg, registryNames(cfg.Registry())) {
		if spec.Logical != "transfer" {
			continue
		}
		if want := []string{"transfer_tokyo"}; !slices.Equal(spec.Physical, want) {
			t.Fatalf("transfer physical queues = %v, want literal %v", spec.Physical, want)
		}
		return
	}
	t.Fatal("transfer queue spec missing")
}

func TestWorkerPeriodicJobSpecs_MaxAttempts(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.CMDetect.Enabled = true

	for _, periodic := range workerPeriodicJobSpecs(cfg, []string{"tokyo", "takamatsu"}) {
		if got := periodic.Args.Spec().MaxAttempts; got != 1 {
			t.Errorf("%s MaxAttempts = %d, want literal 1", periodic.Args.Kind(), got)
		}
	}
}

// cm_detect_reconcile は cm_detect.enabled のときだけ並ぶ（両方向）。
func TestNewWorkerSpec_CMDetectReconcileFollowsCMDetectEnabled(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	has := func() bool {
		for _, p := range newWorkerSpec(cfg, cfg.Registry()).PeriodicJobs {
			if p.Args.Kind() == "cm_detect_reconcile" {
				if p.Interval != 15*time.Minute {
					t.Errorf("cm_detect_reconcile interval = %s, want 15m", p.Interval)
				}
				return true
			}
		}
		return false
	}
	cfg.CMDetect.Enabled = false
	if has() {
		t.Error("cm_detect_reconcile is in the spec with cm_detect.enabled=false")
	}
	cfg.CMDetect.Enabled = true
	if !has() {
		t.Error("cm_detect_reconcile is missing with cm_detect.enabled=true")
	}
}

// epg.sync_interval は config の値が効く（設定キーのある 4 つのうちの 1 つ）。
func TestNewWorkerSpec_EpgSyncIntervalFollowsConfig(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.Epg.SyncInterval = 42 * time.Minute
	found := 0
	for _, p := range newWorkerSpec(cfg, cfg.Registry()).PeriodicJobs {
		if p.Args.Kind() != "epg_sync" {
			continue
		}
		found++
		if p.Interval != 42*time.Minute {
			t.Errorf("epg_sync interval = %s, want 42m", p.Interval)
		}
	}
	if found != 2 {
		t.Errorf("epg_sync entries = %d, want 2 (one per site)", found)
	}
}

// 0 サイト（中央プロセス）: site 単位のキューは物理名が空、site 単位の定期ジョブは無い。
func TestNewWorkerSpec_NoSites(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	spec := newWorkerSpec(cfg, nil)
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, q := range spec.Queues {
		wantEmpty := slices.Contains([]string{"ingest", "epg", "reconciler", "watcher"}, q.Logical)
		if wantEmpty != (len(q.Physical) == 0) {
			t.Errorf("queue %q Physical = %v, empty want %v", q.Logical, q.Physical, wantEmpty)
		}
	}
	if got := len(spec.PeriodicJobs); got != 8 {
		t.Errorf("periodic jobs with 0 sites = %d, want 8 (site-independent only)", got)
	}
}

// 仕様の定期ジョブの kind は、実際の worker 登録で解決できる（cm_detect 込み）。
func TestNewWorkerSpec_PeriodicKindsResolveInWorkerRegistry(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.CMDetect.Enabled = true
	registry := worker.NewWorkers(&worker.Deps{})
	if err := newWorkerSpec(cfg, cfg.Registry()).CheckKinds(registry); err != nil {
		t.Fatalf("CheckKinds: %v", err)
	}
}

// 起動形態（--queues / worker.periodic_jobs / --once / 停止猶予 / 救済閾値）が仕様とは別に写る。
// false でも仕様の一覧は空にならない。
func TestNewWorkerClientConfig_StartupShape(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.Worker.RescueStuckJobsAfter = 3 * time.Minute
	gate := riveradapter.NewOnceGate()
	for _, periodic := range []bool{true, false} {
		cfg.Worker.PeriodicJobs = periodic
		got := newWorkerClientConfig(cfg, cfg.Registry(), []string{"ruler"}, gate, 9*time.Second)
		if got.Once != gate {
			t.Error("Once gate was not passed through")
		}
		if got.RescueStuckJobsAfter != 3*time.Minute {
			t.Errorf("RescueStuckJobsAfter = %s, want 3m", got.RescueStuckJobsAfter)
		}
		if got.SchedulePeriodicJobs != periodic {
			t.Errorf("SchedulePeriodicJobs = %v, want %v", got.SchedulePeriodicJobs, periodic)
		}
		if !slices.Equal(got.Queues, []string{"ruler"}) || got.SoftStopTimeout != 9*time.Second {
			t.Errorf("Queues/SoftStopTimeout = %v / %s", got.Queues, got.SoftStopTimeout)
		}
		if len(got.Spec.PeriodicJobs) != 18 {
			t.Errorf("Spec.PeriodicJobs = %d with periodic_jobs=%v, want 17", len(got.Spec.PeriodicJobs), periodic)
		}
	}
}

// 投入側（jobs.PhysicalQueueFor）と購読側（Spec.Physical）は、定期ジョブの投入先で一致する。
// 論理名をそのまま返す関数とは一致しない --- 両方向。
func TestNewWorkerSpec_PeriodicJobsAreInsertedIntoSubscribedQueues(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	spec := newWorkerSpec(cfg, cfg.Registry())
	if err := spec.CheckQueueFor(jobs.PhysicalQueueFor); err != nil {
		t.Fatalf("CheckQueueFor(jobs.PhysicalQueueFor): %v", err)
	}
	logical := func(args jobq.Args) (string, error) { return args.Spec().Queue, nil }
	if err := spec.CheckQueueFor(logical); err == nil {
		t.Error("CheckQueueFor accepted a queueFor that returns logical names for site-bound jobs")
	}
}
