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
	"github.com/fetburner/rokuban/internal/store"
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
	scoped := newStoreScopedDeps(pool, cfg, base, "test-store", roots["test-store"], nil)
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

func TestStoreScopedDeleteReconcileKeepsOldS3CopyAfterDerivedStoreSwitch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	cfg := &config.Config{Storage: config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"test-store": {Kind: "fs"},
			"s3-a":       {Kind: "s3", Bucket: "old-bucket"},
			"s3-b":       {Kind: "s3", Bucket: "new-bucket"},
		},
		Originals: []config.OriginalStoreRule{{Store: "test-store"}},
		Derived:   "s3-a",
		Transfer:  config.StorageTransferConfig{Overlap: time.Hour},
	}, Mirakcs: []config.MirakcSite{{Site: "default"}}}
	oldFingerprint, err := cfg.Storage.PolicyFingerprint()
	if err != nil {
		t.Fatalf("old storage PolicyFingerprint: %v", err)
	}
	cfg.Storage.Derived = "s3-b"
	newFingerprint, err := cfg.Storage.PolicyFingerprint()
	if err != nil {
		t.Fatalf("new storage PolicyFingerprint: %v", err)
	}
	if newFingerprint == oldFingerprint {
		t.Fatal("PolicyFingerprint did not change when the derived S3 store changed")
	}

	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = store.AssetKindEncoded
		profile := "h264"
		p.Profile = &profile
		p.RelPath = "sites/default/s3-switch/fs-copy.mp4"
		p.SizeBytes = 11
	})
	if _, err := pool.Exec(ctx, `
UPDATE media_asset_copies
SET state = 'deleted', deleted_at = now()
WHERE media_asset_id = $1 AND store = 'test-store'`, assetID); err != nil {
		t.Fatalf("retiring fixture FS copy: %v", err)
	}
	oldRelPath := "sites/default/s3-switch/old-s3-a.mp4"
	newRelPath := "sites/default/s3-switch/new-s3-b.mp4"
	for _, copy := range []struct{ name, relPath string }{{"s3-a", oldRelPath}, {"s3-b", newRelPath}} {
		if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at)
SELECT a.id, $2, $3, COALESCE(a.derived_generation, 0), 'active', now() - interval '3 hours'
FROM media_assets a WHERE a.id = $1`, assetID, copy.name, copy.relPath); err != nil {
			t.Fatalf("adding active copy in %s: %v", copy.name, err)
		}
	}

	// The store-scoped SQL can identify s3-a as prunable once s3-b is active and the overlap has elapsed.
	rules, err := cfg.Storage.OriginalRulesJSON()
	if err != nil {
		t.Fatalf("encoding storage rules: %v", err)
	}
	oldStoreMedia := pgstore.NewMediaWithStores(pool, "s3-a", "test-store", "s3-b")
	candidates, err := oldStoreMedia.ListMediaAssetCopiesToDelete(ctx, rules, "s3-b", time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("listing s3-a placement candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Store != "s3-a" {
		t.Fatalf("s3-a placement candidates after s3-b passed overlap = %+v, want the s3-a copy", candidates)
	}

	fsRoot := t.TempDir()
	if err := mediapath.WriteStoreMarker(fsRoot, "test-store"); err != nil {
		t.Fatalf("writing FS store marker: %v", err)
	}
	fsObjects, err := fsstore.New(fsRoot)
	if err != nil {
		t.Fatalf("creating FS object store: %v", err)
	}
	derivedObjects, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("creating s3-b test object store: %v", err)
	}
	oldS3Root := t.TempDir()
	oldS3Path := filepath.Join(oldS3Root, filepath.FromSlash(oldRelPath))
	if err := os.MkdirAll(filepath.Dir(oldS3Path), 0o755); err != nil {
		t.Fatalf("creating old S3 object directory: %v", err)
	}
	if err := os.WriteFile(oldS3Path, []byte("old s3-a copy"), 0o644); err != nil {
		t.Fatalf("writing old S3 object: %v", err)
	}

	media := pgstore.NewMediaWithStores(pool, "", "", "s3-b")
	base := &worker.Deps{
		Breakers:              pgstore.NewBreakers(pool),
		RecordingPurge:        pgstore.NewRecordings(pool, ""),
		StorageObservation:    media,
		StorageObjects:        map[string]objstore.Store{"test-store": fsObjects, "s3-b": derivedObjects},
		StorageRoots:          map[string]string{"test-store": fsRoot},
		StorageTransfer:       media,
		StorageTransferPolicy: cfg.Storage,
		TransferOverlap:       cfg.Storage.Transfer.Overlap,
		Cleanup:               config.CleanupConfig{MaxDeletesPerPass: 100},
	}
	perStore := newPerStoreWorkerDeps(pool, cfg, base, base.StorageRoots, derivedObjects)
	perStoreNames := make([]string, 0, len(perStore))
	for name := range perStore {
		perStoreNames = append(perStoreNames, name)
	}
	slices.Sort(perStoreNames)
	// 変異候補: configured S3 置き場にも Deps を作ると、下の key assertion に s3-a/s3-b が加わって失敗する。
	if !slices.Equal(perStoreNames, []string{"test-store"}) {
		t.Fatalf("server per-store Deps = %v, want only mounted FS stores", perStoreNames)
	}
	registry := worker.NewWorkers(&worker.Deps{PerStore: perStore})
	for _, name := range perStoreNames {
		if err := registry.Work(ctx, jobs.DeleteReconcileArgs{Store: name}); err != nil {
			t.Fatalf("delete reconcile for server-scoped store %q: %v", name, err)
		}
	}

	var oldState, newState string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 's3-a'`, assetID).Scan(&oldState); err != nil {
		t.Fatalf("reading s3-a copy state: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 's3-b'`, assetID).Scan(&newState); err != nil {
		t.Fatalf("reading s3-b copy state: %v", err)
	}
	if oldState != "active" {
		t.Fatalf("s3-a copy state after server-scoped delete reconcile = %q, want active", oldState)
	}
	if newState != "active" {
		t.Fatalf("current s3-b copy state after delete reconcile = %q, want active", newState)
	}
	if _, err := os.Stat(oldS3Path); err != nil {
		t.Fatalf("old s3-a object after server-scoped delete reconcile: %v", err)
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

// TestStoreScopedDerivedDeletePassOnlyForS3Derived は、置き場ごとの Deps に独立した派生物の削除パス
// （DerivedMediaDelete）を載せるのが、派生物の置き場が S3 のときだけであることを確かめる。
// FS の派生物を別の置き場の worker がマウントしているときは、その FS を objstore のパスで掃かない。
//
// 変異確認: newStoreScopedDeps の kind == "s3" の条件を外すと、FS 派生物のケースで
// DerivedMediaDelete が載り、このテストが落ちる。
func TestStoreScopedDerivedDeletePassOnlyForS3Derived(t *testing.T) {
	derivedObjects, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := &worker.Deps{}

	for _, tc := range []struct {
		name     string
		derived  config.StoreConfig
		wantPass bool
	}{
		{"FS derived store mounted by another store's worker", config.StoreConfig{Kind: "fs"}, false},
		{"S3 derived store", config.StoreConfig{Kind: "s3"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Storage: config.StorageConfig{
				Stores:  map[string]config.StoreConfig{"takamatsu": {Kind: "fs"}, "derived": tc.derived},
				Derived: "derived",
			}, Mirakcs: []config.MirakcSite{{Site: "default"}}}
			scoped := newStoreScopedDeps(nil, cfg, base, "takamatsu", t.TempDir(), derivedObjects)
			if got := scoped.DerivedMediaDelete != nil; got != tc.wantPass {
				t.Errorf("separate derived delete pass configured = %v, want %v", got, tc.wantPass)
			}
		})
	}
}

// TestStoreScopedS3DeletePassHasSingleOwner は、S3 の派生物削除パスを載せる Deps が
// 複数の FS 置き場の中で 1 つだけであり、名前順で決まることを確かめる。
func TestStoreScopedS3DeletePassHasSingleOwner(t *testing.T) {
	derivedObjects, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"tokyo":     {Kind: "fs"},
			"takamatsu": {Kind: "fs"},
			"s3main":    {Kind: "s3"},
		},
		Derived: "s3main",
	}, Mirakcs: []config.MirakcSite{{Site: "default"}}}

	var owners []string
	for _, name := range []string{"tokyo", "takamatsu"} {
		scoped := newStoreScopedDeps(nil, cfg, &worker.Deps{}, name, t.TempDir(), derivedObjects)
		if scoped.DerivedMediaDelete != nil {
			owners = append(owners, name)
		}
	}
	if !slices.Equal(owners, []string{"takamatsu"}) {
		t.Fatalf("S3 derived delete pass owners = %v, want [takamatsu]", owners)
	}
}
