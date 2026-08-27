package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
)

// rokuban enqueue ruler-pass がジョブを投入すること。
func TestRunEnqueue_InsertsJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "ruler-pass", db.DefaultSite, &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}

	if !strings.Contains(out.String(), "inserted job") {
		t.Errorf("output = %q, want to contain %q", out.String(), "inserted job")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'ruler_pass'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting ruler_pass jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("ruler_pass job count = %d, want 1", count)
	}
}

// 既に同じサイトのジョブが待機中なら投入せず、終了コード 0 相当（error が nil）
// であること。CronJob が失敗扱いにならないようにするための挙動
// （docs/data.md §2、UniqueOpts による合流）。
func TestRunEnqueue_AlreadyPending_SkipsWithoutError(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var first bytes.Buffer
	if err := runEnqueue(ctx, pool, "ruler-pass", db.DefaultSite, &first); err != nil {
		t.Fatalf("runEnqueue (first): %v", err)
	}

	var second bytes.Buffer
	if err := runEnqueue(ctx, pool, "ruler-pass", db.DefaultSite, &second); err != nil {
		t.Fatalf("runEnqueue (second) returned error, want nil (合流時も終了コード 0): %v", err)
	}
	if !strings.Contains(second.String(), "already pending") {
		t.Errorf("output = %q, want to contain %q", second.String(), "already pending")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'ruler_pass'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting ruler_pass jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("ruler_pass job count = %d, want 1 (2 回目は合流して増えないはず)", count)
	}
}

// epg-sync も同様に投入できること（epg_sync と ruler_pass の両方が対応していることの
// 確認）。
func TestRunEnqueue_EpgSync(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "epg-sync", db.DefaultSite, &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'epg_sync'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting epg_sync jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("epg_sync job count = %d, want 1", count)
	}
}

// reconcile-pass も投入できること（epg_sync / ruler_pass / reconcile_pass の
// 3 ジョブすべてが rokuban enqueue に対応していることの確認。issue #24 M2-17）。
func TestRunEnqueue_ReconcilePass(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "reconcile-pass", db.DefaultSite, &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'reconcile_pass'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting reconcile_pass jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("reconcile_pass job count = %d, want 1", count)
	}
}

// record-sweep も投入できること（watcher の 3 段構えのうち (c) 定期全量突き合わせを
// ジョブに切り出したことの確認。issue #24 M2-18）。
func TestRunEnqueue_RecordSweep(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "record-sweep", db.DefaultSite, &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'record_sweep'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting record_sweep jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("record_sweep job count = %d, want 1", count)
	}
}

// tuner-sync も投入できること（チューナー射影を CronJob から回せることの確認。
// issue #24 M2-10）。
func TestRunEnqueue_TunerSync(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "tuner-sync", db.DefaultSite, &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'tuner_sync'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting tuner_sync jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("tuner_sync job count = %d, want 1", count)
	}
}

// catalog-export も投入できること（M3-9 / issue #71）。
// site 非依存なので site は空で渡す（issue #200）。
func TestRunEnqueue_CatalogExport(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "catalog-export", "", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	if !strings.Contains(out.String(), "inserted job") {
		t.Errorf("output = %q, want to contain %q", out.String(), "inserted job")
	}
	// site 非依存のログは "for site" を付けない（運用者が効いていると誤解しないため）。
	if strings.Contains(out.String(), "for site") {
		t.Errorf("output = %q, site-independent job must not mention site", out.String())
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'catalog_export'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting catalog_export jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("catalog_export job count = %d, want 1", count)
	}
}

// encode-reconcile も投入できること（issue #163）。`worker.periodic_jobs: false`
// の構成ではこの経路（CronJob → enqueue）が定期パスの唯一の契機になるので、
// 投入できること自体が受け入れ条件の一部になる。
func TestRunEnqueue_EncodeReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "encode-reconcile", "", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	if !strings.Contains(out.String(), "inserted job") {
		t.Errorf("output = %q, want to contain %q", out.String(), "inserted job")
	}
	if strings.Contains(out.String(), "for site") {
		t.Errorf("output = %q, site-independent job must not mention site", out.String())
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'encode_reconcile'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting encode_reconcile jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("encode_reconcile job count = %d, want 1", count)
	}
}

// storage-sync も投入できること（issue #238 M7-5）。catalog-export と同じく
// site 非依存なので site は空で渡す。
func TestRunEnqueue_StorageSync(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "storage-sync", "", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	if !strings.Contains(out.String(), "inserted job") {
		t.Errorf("output = %q, want to contain %q", out.String(), "inserted job")
	}
	if strings.Contains(out.String(), "for site") {
		t.Errorf("output = %q, site-independent job must not mention site", out.String())
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'storage_sync'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting storage_sync jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("storage_sync job count = %d, want 1", count)
	}
}

// delete-reconcile も投入できること。**`worker.periodic_jobs: false`
// （k8s の出荷値）ではこの経路が唯一の投入者**になるので、載っていないと
// ごみ箱・孤児回収のパスが一度も走らない構成ができる。物理ストレージは単一の
// media_dir で site に従属しないので site は空で渡す。
func TestRunEnqueue_DeleteReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "delete-reconcile", "", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	if !strings.Contains(out.String(), "inserted job") {
		t.Errorf("output = %q, want to contain %q", out.String(), "inserted job")
	}
	if strings.Contains(out.String(), "for site") {
		t.Errorf("output = %q, site-independent job must not mention site", out.String())
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'delete_reconcile'`,
	).Scan(&count); err != nil {
		t.Fatalf("counting delete_reconcile jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("delete_reconcile job count = %d, want 1", count)
	}
	// cleanup キューに乗ること（DeleteReconcileArgs.InsertOpts）。KEDA の
	// スケーラはキュー名で引くので、ここがずれると CronJob が投入しても
	// 誰も起きない。
	var queue string
	if err := pool.QueryRow(ctx,
		`SELECT queue FROM river_job WHERE kind = 'delete_reconcile'`,
	).Scan(&queue); err != nil {
		t.Fatalf("reading delete_reconcile queue: %v", err)
	}
	if queue != "cleanup" {
		t.Errorf("delete_reconcile queue = %q, want %q", queue, "cleanup")
	}
}

// 未知のジョブ名はエラーになること。
func TestRunEnqueue_UnknownJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "no-such-job", db.DefaultSite, &out); err == nil {
		t.Fatal("unknown job のとき error を期待したが nil だった")
	}
}

func newEnqueueJobSiteTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("site", "", "")
	return cmd
}

// resolveEnqueueJobSite が site 束縛 / site 非依存を正しく分岐すること（issue #200）。
func TestResolveEnqueueJobSite(t *testing.T) {
	multi := []config.MirakcSite{
		{Site: "tokyo"},
		{Site: "takamatsu"},
	}

	t.Run("site-independent without --site succeeds with empty site even under multi-site registry", func(t *testing.T) {
		cmd := newEnqueueJobSiteTestCmd(t)
		site, err := resolveEnqueueJobSite(cmd, "catalog-export", multi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if site != "" {
			t.Errorf("site = %q, want empty for site-independent job", site)
		}
	})

	t.Run("site-independent with --site is an error (not silently ignored)", func(t *testing.T) {
		cmd := newEnqueueJobSiteTestCmd(t)
		if err := cmd.Flags().Set("site", "tokyo"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		_, err := resolveEnqueueJobSite(cmd, "catalog-export", multi)
		if err == nil {
			t.Fatal("expected error when --site is passed to site-independent job, got nil")
		}
		if !strings.Contains(err.Error(), "site-independent") {
			t.Errorf("error = %v, want to mention site-independent", err)
		}
	})

	t.Run("site-independent with empty --site= is still an error (Changed)", func(t *testing.T) {
		cmd := newEnqueueJobSiteTestCmd(t)
		if err := cmd.Flags().Set("site", ""); err != nil {
			t.Fatalf("Set: %v", err)
		}
		_, err := resolveEnqueueJobSite(cmd, "catalog-export", multi)
		if err == nil {
			t.Fatal("expected error when --site is explicitly set (even to empty), got nil")
		}
	})

	t.Run("site-bound without --site under multi-site registry is an error", func(t *testing.T) {
		cmd := newEnqueueJobSiteTestCmd(t)
		_, err := resolveEnqueueJobSite(cmd, "ruler-pass", multi)
		if err == nil {
			t.Fatal("expected --site required error for site-bound job, got nil")
		}
		if !strings.Contains(err.Error(), "--site is required") {
			t.Errorf("error = %v, want --site is required", err)
		}
	})

	t.Run("site-bound with --site resolves", func(t *testing.T) {
		cmd := newEnqueueJobSiteTestCmd(t)
		if err := cmd.Flags().Set("site", "takamatsu"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		site, err := resolveEnqueueJobSite(cmd, "epg-sync", multi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if site != "takamatsu" {
			t.Errorf("site = %q, want takamatsu", site)
		}
	})

	t.Run("unknown job is an error", func(t *testing.T) {
		cmd := newEnqueueJobSiteTestCmd(t)
		_, err := resolveEnqueueJobSite(cmd, "no-such-job", multi)
		if err == nil {
			t.Fatal("expected error for unknown job, got nil")
		}
	})
}

// enqueueJobs の分類が一貫していること。RequiresSite の集合が「次にジョブを
// 足す人がどちらかを更新し忘れる」経路にならないよう、現状の契約を固定する
// （issue #200）。catalog-export と storage-sync（issue #238 M7-5）、
// encode-reconcile（issue #163）、delete-reconcile が site 非依存。
func TestEnqueueJobs_SiteClassification(t *testing.T) {
	independent := sortedJobNamesBySite(false)
	wantIndependent := []string{"catalog-export", "delete-reconcile", "encode-reconcile", "storage-sync"}
	if strings.Join(independent, ",") != strings.Join(wantIndependent, ",") {
		t.Errorf("site-independent jobs = %v, want %v", independent, wantIndependent)
	}

	bound := sortedJobNamesBySite(true)
	wantBound := []string{"epg-sync", "reconcile-pass", "record-sweep", "ruler-pass", "tuner-sync"}
	if strings.Join(bound, ",") != strings.Join(wantBound, ",") {
		t.Errorf("site-bound jobs = %v, want %v", bound, wantBound)
	}

	// 全ジョブがどちらかに属する（漏れなし）。
	if got, want := len(bound)+len(independent), len(enqueueJobs); got != want {
		t.Errorf("bound+independent = %d, enqueueJobs = %d", got, want)
	}
}
