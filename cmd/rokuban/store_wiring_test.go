package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// enqueueRecorder は投入された Args を記録する。
type enqueueRecorder struct {
	mu   sync.Mutex
	args []jobq.Args
}

func (r *enqueueRecorder) Enqueue(_ context.Context, args jobq.Args, _ ...jobq.EnqueueOpt) (jobq.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, args)
	return jobq.Result{}, nil
}

// TestPeriodicThumbnailReconcileEnqueuesWithUnboundMedia は、buildWorkerAdapter と同じ
// 束縛の無い Media（置き場の名前を持たない）で定期パスを回したとき、原本のコピーがある
// 置き場へ thumbnail と seek_tiles のジョブが積まれることを確かめる。
func TestPeriodicThumbnailReconcileEnqueuesWithUnboundMedia(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/unbound/original.ts"
		p.SizeBytes = 1
	})

	media := pgstore.NewMediaWithStores(pool, "", "", dbtest.StoreName)
	enq := &enqueueRecorder{}
	w := &worker.ThumbnailReconcileWorker{Media: media, Enqueuer: enq}
	if err := w.Work(context.Background(), jobs.ThumbnailReconcileArgs{}); err != nil {
		t.Fatalf("ThumbnailReconcileWorker.Work: %v", err)
	}

	var thumbnails []jobs.ThumbnailJobArgs
	var seekTiles []jobs.SeekTilesJobArgs
	for _, args := range enq.args {
		switch v := args.(type) {
		case jobs.ThumbnailJobArgs:
			thumbnails = append(thumbnails, v)
		case jobs.SeekTilesJobArgs:
			seekTiles = append(seekTiles, v)
		}
	}
	wantThumbnails := []jobs.ThumbnailJobArgs{{RecordingID: recordingID, Store: dbtest.StoreName}}
	if !slices.Equal(thumbnails, wantThumbnails) {
		t.Errorf("thumbnail jobs after the periodic pass = %+v, want %+v", thumbnails, wantThumbnails)
	}
	wantSeekTiles := []jobs.SeekTilesJobArgs{{RecordingID: recordingID, Store: dbtest.StoreName}}
	if !slices.Equal(seekTiles, wantSeekTiles) {
		t.Errorf("seek_tiles jobs after the periodic pass = %+v, want %+v", seekTiles, wantSeekTiles)
	}
}

// TestStoreScopedDeleteReconcileDeletesNonDesiredCopyAfterOverlap は、newStoreScopedDeps が作った
// 置き場ごとの削除 reconcile が、その置き場の不要なコピーを overlap の経過後に消すことを確かめる。
func TestStoreScopedDeleteReconcileDeletesNonDesiredCopyAfterOverlap(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	roots := make(map[string]string, 2)
	objects := make(map[string]objstore.Store, 2)
	for _, name := range []string{"test-store", "archive"} {
		root := t.TempDir()
		if err := mediapath.WriteStoreMarker(root, name); err != nil {
			t.Fatalf("writing %s store marker: %v", name, err)
		}
		fs, err := fsstore.New(root)
		if err != nil {
			t.Fatalf("creating %s object store: %v", name, err)
		}
		roots[name], objects[name] = root, fs
	}
	cfg := &config.Config{Storage: config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"test-store": {Kind: "fs"}, "archive": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "archive"}},
		Derived:   "archive",
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}, Mirakcs: []config.MirakcSite{{Site: "default"}}}

	relPath := "sites/default/scoped/source.ts"
	content := []byte("scoped bytes")
	for _, name := range []string{"test-store", "archive"} {
		full := filepath.Join(roots[name], filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = relPath
		p.SizeBytes = int64(len(content))
	})
	// desired の archive コピーは overlap より古い。non-desired の test-store コピーは dbtest が作った active 行。
	if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at)
SELECT a.id, 'archive', $2, COALESCE(a.derived_generation, 0), 'active', now() - interval '3 hours'
FROM media_assets a WHERE a.id = $1`, assetID, relPath); err != nil {
		t.Fatalf("adding archive copy: %v", err)
	}

	// buildWorkerAdapter と同じく、base の StorageTransfer は束縛の無い Media。
	base := &worker.Deps{
		Breakers:              pgstore.NewBreakers(pool),
		StorageObjects:        objects,
		StorageRoots:          roots,
		StorageTransfer:       pgstore.NewMediaWithStores(pool, "", "", cfg.Storage.Derived),
		StorageTransferPolicy: cfg.Storage,
		TransferOverlap:       cfg.Storage.Transfer.Overlap,
		Cleanup:               config.CleanupConfig{MaxDeletesPerPass: 100},
	}
	scoped, err := newStoreScopedDeps(ctx, pool, cfg, base, "test-store", roots["test-store"], nil)
	if err != nil {
		t.Fatalf("newStoreScopedDeps: %v", err)
	}
	registry := worker.NewWorkers(&worker.Deps{PerStore: map[string]*worker.Deps{"test-store": scoped}})
	if err := registry.Work(ctx, jobs.DeleteReconcileArgs{Store: "test-store"}); err != nil {
		t.Fatalf("delete reconcile for test-store: %v", err)
	}

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'test-store' ORDER BY id DESC LIMIT 1`, assetID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "deleted" {
		t.Errorf("test-store copy state after the delete reconcile = %q, want deleted", state)
	}
	if _, err := os.Stat(filepath.Join(roots["test-store"], filepath.FromSlash(relPath))); err == nil {
		t.Error("non-desired file in test-store remains after the delete reconcile")
	}
}

// TestWorkerWithoutMountsRefusesStorelessJobs は、置き場を 1 つもマウントしていないワーカー
// （--stores 無し相当で PerStore が空）に、置き場の無い旧ジョブが来たとき、空の MediaDir で
// 実行されず置き場の不一致として失敗し、メトリクスが増えることを確かめる。
func TestWorkerWithoutMountsRefusesStorelessJobs(t *testing.T) {
	registry := worker.NewWorkers(&worker.Deps{PerStore: map[string]*worker.Deps{}})
	before := promtestutil.ToFloat64(metrics.StoreRouteMismatches.WithLabelValues("delete_reconcile", ""))

	if err := registry.Work(context.Background(), jobs.DeleteReconcileArgs{}); err == nil {
		t.Fatal("Work on a worker with no mounted stores succeeded, want a store route mismatch")
	}
	if after := promtestutil.ToFloat64(metrics.StoreRouteMismatches.WithLabelValues("delete_reconcile", "")); after-before != 1 {
		t.Errorf("store route mismatch counter moved by %v, want 1", after-before)
	}
}
