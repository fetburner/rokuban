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
		t.Fatalf("catalog files = %d, want 1", len(entries))
	}
	if filepath.Ext(entries[0].Name()) != ".json" {
		t.Errorf("file = %q, want .json", entries[0].Name())
	}

	// JSON が読めて version が載っていること。
	doc, err := catalog.Load(filepath.Join(catalog.Dir(mediaDir), entries[0].Name()))
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
func TestCatalogExportArgs_KindAndQueue(t *testing.T) {
	args := CatalogExportArgs{}
	if args.Kind() != "catalog_export" {
		t.Errorf("Kind = %q", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != river.QueueDefault {
		t.Errorf("Queue = %q, want default", opts.Queue)
	}
	// UniqueOpts が設定されていること（空 args の同時実行を防ぐ）。
	if !opts.UniqueOpts.ByArgs {
		t.Error("ByArgs should be true")
	}
}
