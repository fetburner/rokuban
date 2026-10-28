package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestDeleteReconcileAuditIsScopedToMountedStore(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	storeA, rootA := newStoreRootForAuditTest(t, "store-a")
	storeB, rootB := newStoreRootForAuditTest(t, "store-b")

	sharedRelPath := "sites/default/shared.ts"
	bOnlyRelPath := "sites/default/store-b-only.ts"
	aMissingRelPath := "sites/default/store-a-missing.ts"
	writeOldAuditFile(t, rootA, sharedRelPath)
	writeOldAuditFile(t, rootB, sharedRelPath)
	writeOldAuditFile(t, rootB, bOnlyRelPath)

	sharedAssetID, _ := seedStoreScopedOriginalForAudit(t, pool, storeB, sharedRelPath)
	bOnlyAssetID, _ := seedStoreScopedOriginalForAudit(t, pool, storeB, bOnlyRelPath)
	aMissingAssetID, _ := seedStoreScopedOriginalForAudit(t, pool, storeA, aMissingRelPath)

	worker := newStoreScopedDeleteWorker(pool, storeA, rootA)
	worker.OrphanMTimeGrace = time.Hour
	worker.OrphanAge = 24 * time.Hour
	worker.MissingAssetAge = 24 * time.Hour
	if err := worker.Work(ctx, jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work(): %v", err)
	}

	if got := countStoreOrphan(t, pool, storeA, sharedRelPath); got != 1 {
		t.Errorf("store-a orphan observation for a path copied only in store-b = %d, want 1", got)
	}
	if got := countStoreOrphan(t, pool, storeA, bOnlyRelPath); got != 0 {
		t.Errorf("store-a orphan observation for a file present only in store-b = %d, want 0", got)
	}
	if got := countStoreMissingAsset(t, pool, storeA, sharedAssetID); got != 0 {
		t.Errorf("store-a missing observation for store-b shared asset = %d, want 0", got)
	}
	if got := countStoreMissingAsset(t, pool, storeA, bOnlyAssetID); got != 0 {
		t.Errorf("store-a missing observation for store-b-only asset = %d, want 0", got)
	}
	if got := countStoreMissingAsset(t, pool, storeA, aMissingAssetID); got != 1 {
		t.Errorf("store-a missing observation for its own absent asset = %d, want 1", got)
	}
}

func TestDeleteReconcileBreakerIsScopedToMountedStore(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	storeA, rootA := newStoreRootForAuditTest(t, "store-a")
	storeB, rootB := newStoreRootForAuditTest(t, "store-b")
	pathA, pathB := "sites/default/a.ts", "sites/default/b.ts"
	writeOldAuditFile(t, rootA, pathA)
	// The path can exist independently under both roots. Store-b must not delete
	// this file just because the matching DB asset belongs to store-a.
	writeOldAuditFile(t, rootB, pathA)
	writeOldAuditFile(t, rootB, pathB)
	assetA, recordingA := seedStoreScopedOriginalForAudit(t, pool, storeA, pathA)
	assetB, recordingB := seedStoreScopedOriginalForAudit(t, pool, storeB, pathB)
	for _, id := range []int64{recordingA, recordingB} {
		if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
			t.Fatalf("moving recording %d to trash: %v", id, err)
		}
	}

	breakers := pgstoretest.Breakers(pool)
	if err := breaker.Trip(ctx, breakers, storeA, breaker.DeleteReconcile, 1, breaker.Sample{Total: 1}); err != nil {
		t.Fatalf("tripping store-a delete breaker: %v", err)
	}
	workerA := newStoreScopedDeleteWorker(pool, storeA, rootA)
	workerB := newStoreScopedDeleteWorker(pool, storeB, rootB)
	for _, worker := range []*DeleteReconcileWorker{workerA, workerB} {
		worker.TrashRetention = time.Hour
		worker.MaxDeletesPerPass = 10
	}
	if err := workerA.Work(ctx, jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("store-a DeleteReconcileWorker.Work(): %v", err)
	}
	if got := storeCopyState(t, pool, storeA, assetA); got != "active" {
		t.Errorf("store-a copy state with store-a breaker tripped = %q, want active", got)
	}
	if _, err := os.Stat(filepath.Join(rootA, filepath.FromSlash(pathA))); err != nil {
		t.Errorf("store-a file after its breaker stopped deletion: %v", err)
	}
	if err := workerB.Work(ctx, jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("store-b DeleteReconcileWorker.Work(): %v", err)
	}
	if got := storeCopyState(t, pool, storeB, assetB); got != "deleted" {
		t.Errorf("store-b copy state while store-a breaker is tripped = %q, want deleted", got)
	}
	if got := storeCopyState(t, pool, storeA, assetA); got != "active" {
		t.Errorf("store-a copy state after store-b reconcile = %q, want active", got)
	}
	if _, err := os.Stat(filepath.Join(rootA, filepath.FromSlash(pathA))); err != nil {
		t.Errorf("store-a file after store-b reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(pathA))); err != nil {
		t.Errorf("store-b file whose matching DB asset belongs to store-a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(pathB))); !os.IsNotExist(err) {
		t.Errorf("store-b file after its untripped reconcile, stat error = %v, want not-exist", err)
	}
	if tripped, err := breakers.IsTripped(ctx, storeA, breaker.DeleteReconcile); err != nil || !tripped {
		t.Errorf("store-a breaker = (%t, %v), want (true, nil)", tripped, err)
	}
	if tripped, err := breakers.IsTripped(ctx, storeB, breaker.DeleteReconcile); err != nil || tripped {
		t.Errorf("store-b breaker = (%t, %v), want (false, nil)", tripped, err)
	}

	if err := breakers.Resume(ctx, storeA, breaker.DeleteReconcile); err != nil {
		t.Fatalf("resuming store-a breaker: %v", err)
	}
	if err := workerA.Work(ctx, jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("store-a DeleteReconcileWorker.Work() after resume: %v", err)
	}
	if got := storeCopyState(t, pool, storeA, assetA); got != "deleted" {
		t.Errorf("store-a copy state after resume = %q, want deleted", got)
	}
}

func TestDeleteReconcileHonorsLegacyEmptyScopeBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	storeName, root := newStoreRootForAuditTest(t, "store-a")
	relPath := "sites/default/legacy-latched.ts"
	writeOldAuditFile(t, root, relPath)
	assetID, recordingID := seedStoreScopedOriginalForAudit(t, pool, storeName, relPath)
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() - interval '2 hours' WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("moving recording %d to trash: %v", recordingID, err)
	}

	// Before the scope migration, delete_reconcile breakers were stored with an
	// empty scope. That latch must remain effective until an operator resumes it.
	breakers := pgstoretest.Breakers(pool)
	if err := breaker.Trip(ctx, breakers, "", breaker.DeleteReconcile, 1, breaker.Sample{Total: 1}); err != nil {
		t.Fatalf("tripping legacy delete breaker: %v", err)
	}
	worker := newStoreScopedDeleteWorker(pool, storeName, root)
	worker.TrashRetention = time.Hour
	worker.MaxDeletesPerPass = 10
	if err := worker.Work(ctx, jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work(): %v", err)
	}

	if got := storeCopyState(t, pool, storeName, assetID); got != "active" {
		t.Errorf("copy state while legacy breaker is tripped = %q, want active", got)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); err != nil {
		t.Errorf("file while legacy breaker is tripped: %v", err)
	}
	if tripped, err := breakers.IsTripped(ctx, "", breaker.DeleteReconcile); err != nil || !tripped {
		t.Errorf("legacy breaker = (%t, %v), want (true, nil)", tripped, err)
	}
}

func newStoreRootForAuditTest(t *testing.T, storeName string) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := mediapath.WriteStoreMarker(root, storeName); err != nil {
		t.Fatalf("writing %s store marker: %v", storeName, err)
	}
	return storeName, root
}

func writeOldAuditFile(t *testing.T, root, relPath string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", relPath, err)
	}
	if err := os.WriteFile(path, []byte("audit fixture"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", relPath, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("aging %s: %v", relPath, err)
	}
}

func seedStoreScopedOriginalForAudit(t *testing.T, pool *pgxpool.Pool, storeName, relPath string) (int64, int64) {
	t.Helper()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = relPath
	})
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET store = $1 WHERE media_asset_id = $2`, storeName, assetID); err != nil {
		t.Fatalf("assigning asset %d to %s: %v", assetID, storeName, err)
	}
	return assetID, recordingID
}

func newStoreScopedDeleteWorker(pool *pgxpool.Pool, storeName, root string) *DeleteReconcileWorker {
	media := pgstore.NewMedia(pool, storeName, "")
	return &DeleteReconcileWorker{
		MediaDelete: media, Storage: media, RecordingPurge: pgstore.NewRecordings(pool, storeName),
		Breakers: pgstoretest.Breakers(pool), MediaDir: root, MediaStoreName: storeName,
	}
}

func countStoreOrphan(t *testing.T, pool *pgxpool.Pool, storeName, relPath string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM orphan_files WHERE store = $1 AND rel_path = $2`, storeName, relPath).Scan(&count); err != nil {
		t.Fatalf("counting %s orphan %s: %v", storeName, relPath, err)
	}
	return count
}

func countStoreMissingAsset(t *testing.T, pool *pgxpool.Pool, storeName string, assetID int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM missing_media_assets WHERE store = $1 AND media_asset_id = $2`, storeName, assetID).Scan(&count); err != nil {
		t.Fatalf("counting %s missing asset %d: %v", storeName, assetID, err)
	}
	return count
}

func storeCopyState(t *testing.T, pool *pgxpool.Pool, storeName string, assetID int64) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE store = $1 AND media_asset_id = $2`, storeName, assetID).Scan(&state); err != nil {
		t.Fatalf("reading %s copy state for asset %d: %v", storeName, assetID, err)
	}
	return state
}
