package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/testutil"
)

// CatalogExport が media_dir/catalog/ に JSON を書き出すこと。
func TestCatalogExportWorker_WritesFile(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool, MediaDir: mediaDir})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()
	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	if _, err := client.Insert(ctx, CatalogExportArgs{}, nil); err != nil {
		t.Fatalf("inserting catalog_export: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "catalog_export" {
			t.Fatalf("job kind = %q, want catalog_export", event.Job.Kind)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for catalog_export completion")
	}

	entries, err := os.ReadDir(catalog.Dir(mediaDir))
	if err != nil {
		t.Fatalf("reading catalog dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("catalog entries = %d, want 1", len(entries))
	}
	if !entries[0].IsDir() {
		t.Fatalf("entry = %q, want a generation directory", entries[0].Name())
	}

	// 世代が完成宣言（manifest）まで書けていること = rescue が選べる形で
	// 出ていること（docs/storage.md §8）。
	genDir := filepath.Join(catalog.Dir(mediaDir), entries[0].Name())
	if _, err := catalog.VerifyGeneration(genDir); err != nil {
		t.Fatalf("exported generation does not verify: %v", err)
	}

	// JSON が読めて version が載っていること。
	doc, err := catalog.Load(filepath.Join(genDir, catalog.DocumentFilename))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Version != catalog.Version {
		t.Errorf("version = %d, want %d", doc.Version, catalog.Version)
	}
}

// CatalogExport が PeriodicJobs に載ること。
func TestBuildRiverConfig_CatalogExportPeriodic(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:  true,
		CatalogExport: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 1 {
		t.Fatalf("PeriodicJobs = %d, want 1 (catalog_export only)", len(riverCfg.PeriodicJobs))
	}
}

// Kind / InsertOpts の形。
//
// Queue の期待値はリテラル "cleanup" で書く（cleanupQueue 定数と比較すると、
// InsertOpts の実装も同じ定数を参照しているだけなので、定数の値が何であっても
// 常に一致してしまい何も主張しない。docs/overview.md のキュー配置表が
// 約束している実際の名前と一致するかを確認したい）。
func TestCatalogExportArgs_KindAndQueue(t *testing.T) {
	args := CatalogExportArgs{}
	if args.Kind() != "catalog_export" {
		t.Errorf("Kind = %q", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != "cleanup" {
		t.Errorf("Queue = %q, want %q", opts.Queue, "cleanup")
	}
	// UniqueOpts が設定されていること（空 args の同時実行を防ぐ）。
	if !opts.UniqueOpts.ByArgs {
		t.Error("ByArgs should be true")
	}
	// ByQueue が立っていること（issue #185 レビュー: キュー名の変更が一意キーに
	// 影響しないと、旧キュー（river.QueueDefault）の残骸が新キュー（cleanup）への
	// insert を UniqueSkippedAsDuplicate として黙って塞ぐ。internal/jobs/queue.go の
	// UniqueByQueue の doc コメント参照）。
	if !opts.UniqueOpts.ByQueue {
		t.Error("ByQueue should be true (キュー名変更が一意キーに影響しないと旧キューの残骸が新キューへの insert を塞ぐ)")
	}
}
