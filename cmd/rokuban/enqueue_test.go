package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
func TestRunEnqueue_CatalogExport(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "catalog-export", db.DefaultSite, &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
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

// 未知のジョブ名はエラーになること。
func TestRunEnqueue_UnknownJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "no-such-job", db.DefaultSite, &out); err == nil {
		t.Fatal("unknown job のとき error を期待したが nil だった")
	}
}
