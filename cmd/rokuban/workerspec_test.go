package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/tsscan"
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
	spec := newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames())
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
		{Logical: "encode", Physical: []string{"encode_test-store"}, MaxWorkers: 2},
		{Logical: "thumbnail", Physical: []string{"thumbnail_test-store"}, MaxWorkers: 3},
		{Logical: "cm_detect", Physical: []string{"cm_detect_test-store"}, MaxWorkers: 1},
		{Logical: "cleanup", Physical: []string{"cleanup"}, MaxWorkers: 2},
		{Logical: "storage", Physical: []string{"storage_test-store"}, MaxWorkers: 1},
		{Logical: "transfer", Physical: []string{"transfer_test-store"}, MaxWorkers: 1},
		{Logical: "original", Physical: []string{"original_test-store"}, MaxWorkers: 2},
		{Logical: "ts_scan", Physical: []string{"ts_scan_test-store"}, MaxWorkers: 1},
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
	for _, q := range newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames()).Queues {
		got = append(got, q.Logical)
	}
	slices.Sort(got)
	if want := jobs.AllQueueNames(); !slices.Equal(got, want) {
		t.Errorf("logical queues = %v, want jobs.AllQueueNames() = %v", got, want)
	}
}

func TestNewWorkerSpec_PeriodicJobs(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	got := periodicGot(newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames()))
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
		{"catalog_export", "", 24 * time.Hour, "original"},
		{"delete_reconcile", "", 15 * time.Minute, "original"},
		{"storage_sync", "", 5 * time.Minute, "storage"},
		{"label_rule_reconcile", "", 15 * time.Minute, "cleanup"},
		{"encode_reconcile", "", 15 * time.Minute, "cleanup"},
		{"thumbnail_reconcile", "", 15 * time.Minute, "cleanup"},
		{"ts_scan_reconcile", "", 15 * time.Minute, "cleanup"},
		{"storage_transfer_reconcile", "", 15 * time.Minute, "cleanup"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("periodic jobs =\n%v\nwant\n%v", got, want)
	}
}

// 置き場で修飾するキューの物理名は、config の置き場の集合ではなく、--stores でマウントした置き場で決まる。
// 取り違えると、マウントしていない置き場のキューを購読して何も掴めない worker になる。
// 期待値はリテラルで書く（実装の定数を使うと、どの論理キューが修飾されるかを何も固定しない）。
func TestWorkerQueueSpecsQualifyByMountedFSStore(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.Storage.Stores = map[string]config.StoreConfig{
		"tokyo":     {Kind: "fs"},
		"takamatsu": {Kind: "fs"},
		"s3main":    {Kind: "s3"},
	}
	storeBound := []string{"transfer", "encode", "thumbnail", "cm_detect", "ts_scan", "original", "storage"}
	tests := []struct {
		name    string
		mounted []string
		want    func(logical string) []string
	}{
		{name: "mounted tokyo", mounted: []string{"tokyo"}, want: func(l string) []string { return []string{l + "_tokyo"} }},
		{name: "mounted takamatsu", mounted: []string{"takamatsu"}, want: func(l string) []string { return []string{l + "_takamatsu"} }},
		{name: "mounted both", mounted: []string{"tokyo", "takamatsu"}, want: func(l string) []string { return []string{l + "_tokyo", l + "_takamatsu"} }},
		{name: "mounts none", mounted: nil, want: func(string) []string { return []string{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := 0
			for _, spec := range workerQueueSpecs(cfg, registryNames(cfg.Registry()), tt.mounted) {
				if !slices.Contains(storeBound, spec.Logical) {
					continue
				}
				seen++
				if want := tt.want(spec.Logical); !slices.Equal(spec.Physical, want) {
					t.Errorf("%s physical queues = %v, want literal %v", spec.Logical, spec.Physical, want)
				}
			}
			if seen != len(storeBound) {
				t.Fatalf("saw %d store-bound queue specs, want %d", seen, len(storeBound))
			}
		})
	}
}

// 置き場で修飾するかの判定は jobs.IsStoreBound の 1 か所で、投入側の PhysicalQueueFor と購読側の worker 仕様が
// 食い違わない。論理キューごとに、store を持つ Args の投入先が購読している物理キューに含まれることを確かめる。
func TestStoreBoundJobsAreInsertedIntoSubscribedQueues(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.CMDetect.Enabled = true
	mounted := []string{"tokyo", "takamatsu"}
	spec := newWorkerSpec(cfg, cfg.Registry(), mounted)
	subscribed := map[string]bool{}
	for _, q := range spec.Queues {
		for _, physical := range q.Physical {
			subscribed[physical] = true
		}
	}
	for _, args := range []jobq.Args{
		jobs.EncodeJobArgs{Store: "takamatsu"}, jobs.ThumbnailJobArgs{Store: "takamatsu"}, jobs.SeekTilesJobArgs{Store: "takamatsu"},
		jobs.CMDetectJobArgs{Store: "takamatsu"}, jobs.CMLogoCandidateJobArgs{Store: "takamatsu"},
		tsscan.ScanArgs{Store: "takamatsu"}, jobs.DeleteReconcileArgs{Store: "takamatsu"}, jobs.CatalogExportArgs{Store: "takamatsu"},
		jobs.StorageSyncArgs{Store: "takamatsu"}, jobs.StorageTransferArgs{Store: "takamatsu"},
	} {
		got, err := jobs.PhysicalQueueFor(args)
		if err != nil {
			t.Fatalf("PhysicalQueueFor(%T): %v", args, err)
		}
		if want := args.Spec().Queue + "_takamatsu"; got != want {
			t.Errorf("PhysicalQueueFor(%T) = %q, want literal-derived %q", args, got, want)
		}
		if !subscribed[got] {
			t.Errorf("%T is inserted into %q, which the worker does not subscribe to", args, got)
		}
	}
}

func TestWorkerPeriodicJobSpecs_MaxAttempts(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.CMDetect.Enabled = true

	for _, periodic := range workerPeriodicJobSpecs(cfg, []string{"tokyo", "takamatsu"}, nil) {
		if got := periodic.Args.Spec().MaxAttempts; got != 1 {
			t.Errorf("%s MaxAttempts = %d, want literal 1", periodic.Args.Kind(), got)
		}
	}
}

// cm_detect_reconcile は cm_detect.enabled のときだけ並ぶ（両方向）。
func TestNewWorkerSpec_CMDetectReconcileFollowsCMDetectEnabled(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	has := func() bool {
		for _, p := range newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames()).PeriodicJobs {
			if p.Args.Kind() == "cm_detect_reconcile" {
				if got := p.Args.Spec().Queue; got != "cleanup" {
					t.Errorf("cm_detect_reconcile queue = %q, want literal cleanup", got)
				}
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

type blockingEncodeQueueWorker struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (w blockingEncodeQueueWorker) Work(ctx context.Context, _ jobs.EncodeJobArgs) error {
	select {
	case w.started <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type signalEncodeReconcileWorker struct{ ran chan<- struct{} }

func (w signalEncodeReconcileWorker) Work(context.Context, jobs.EncodeReconcileArgs) error {
	w.ran <- struct{}{}
	return nil
}

// 長時間 encode が monolith の encode 並列数を使い切っても、reconcile は別キューで実行できる。
// Spec の encode_reconcile を encode に戻すと全 worker が塞がり、このテストは期限内に完了しない。
func TestEncodeReconcileRunsWhileEncodeQueueIsFull(t *testing.T) {
	pool := testutil.SetupDB(t)
	cfg := workerSpecTestConfig(t)
	stores := cfg.Storage.FSStoreNames()
	if len(stores) == 0 {
		t.Fatal("test config has no fs store; encode is store-qualified")
	}
	store := stores[0]
	var productSpec jobq.WorkerSpec
	for _, queue := range newWorkerSpec(cfg, nil, []string{store}).Queues {
		if queue.Logical == "encode" || queue.Logical == "cleanup" {
			productSpec.Queues = append(productSpec.Queues, queue)
		}
	}
	if len(productSpec.Queues) != 2 {
		t.Fatalf("selected queue specs = %v, want encode and cleanup", productSpec.Queues)
	}
	encodeWorkers := 0
	for _, queue := range productSpec.Queues {
		if queue.Logical == "encode" {
			encodeWorkers = queue.MaxWorkers
		}
	}
	if encodeWorkers < 1 {
		t.Fatalf("encode MaxWorkers = %d, want a positive configured concurrency", encodeWorkers)
	}

	started := make(chan struct{}, encodeWorkers)
	release := make(chan struct{})
	reconcileRan := make(chan struct{}, 1)
	registry := jobq.NewRegistry()
	riveradapter.Register(registry, blockingEncodeQueueWorker{started: started, release: release})
	riveradapter.Register(registry, signalEncodeReconcileWorker{ran: reconcileRan})
	adapter, err := riveradapter.NewAdapter(pool, registry, jobs.PhysicalQueueFor, riveradapter.Config{Spec: productSpec})
	if err != nil {
		t.Fatalf("creating adapter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := adapter.Start(ctx); err != nil {
		cancel()
		t.Fatalf("starting adapter: %v", err)
	}
	defer func() {
		close(release)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if err := adapter.Stop(stopCtx); err != nil {
			t.Errorf("stopping adapter: %v", err)
		}
		cancel()
	}()

	for i := 0; i < encodeWorkers; i++ {
		args := jobs.EncodeJobArgs{Store: store, RecordingID: int64(i + 1), Profile: "test", Timeout: time.Hour}
		if _, err := adapter.Enqueue(ctx, args); err != nil {
			t.Fatalf("enqueueing blocking encode %d: %v", i+1, err)
		}
	}
	for i := 0; i < encodeWorkers; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d encode workers started", i, encodeWorkers)
		}
	}

	if _, err := adapter.Enqueue(ctx, jobs.EncodeReconcileArgs{}); err != nil {
		t.Fatalf("enqueueing encode reconcile: %v", err)
	}
	select {
	case <-reconcileRan:
	case <-time.After(10 * time.Second):
		t.Fatal("encode_reconcile did not run while the encode queue was full")
	}
}

// epg.sync_interval は config の値が効く（設定キーのある 4 つのうちの 1 つ）。
func TestNewWorkerSpec_EpgSyncIntervalFollowsConfig(t *testing.T) {
	cfg := workerSpecTestConfig(t)
	cfg.Epg.SyncInterval = 42 * time.Minute
	found := 0
	for _, p := range newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames()).PeriodicJobs {
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
	spec := newWorkerSpec(cfg, nil, cfg.Storage.FSStoreNames())
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, q := range spec.Queues {
		wantEmpty := slices.Contains([]string{"ingest", "epg", "reconciler", "watcher"}, q.Logical)
		if slices.Contains([]string{"transfer", "encode", "thumbnail", "cm_detect", "ts_scan", "original", "storage"}, q.Logical) {
			wantEmpty = false // 置き場で分けるキューは、0 サイトでもマウントした置き場の数だけある
		}
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
	if err := newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames()).CheckKinds(registry); err != nil {
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
		got := newWorkerClientConfig(cfg, cfg.Registry(), cfg.Storage.FSStoreNames(), []string{"ruler"}, gate, 9*time.Second)
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
	spec := newWorkerSpec(cfg, cfg.Registry(), cfg.Storage.FSStoreNames())
	if err := spec.CheckQueueFor(jobs.PhysicalQueueFor); err != nil {
		t.Fatalf("CheckQueueFor(jobs.PhysicalQueueFor): %v", err)
	}
	logical := func(args jobq.Args) (string, error) { return args.Spec().Queue, nil }
	if err := spec.CheckQueueFor(logical); err == nil {
		t.Error("CheckQueueFor accepted a queueFor that returns logical names for site-bound jobs")
	}
}
