package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// placementDeleteWorker は 1 つの置き場に束縛した削除 reconcile worker を返す。
func placementDeleteWorker(pool *pgxpool.Pool, cfg config.StorageConfig, roots map[string]string, storeName string) *DeleteReconcileWorker {
	media := pgstore.NewMedia(pool, storeName, "")
	return &DeleteReconcileWorker{
		MediaDelete: media, Storage: media, StorageTransfer: media, StorageTransferPolicy: cfg,
		RecordingPurge: pgstore.NewRecordings(pool, storeName), Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots[storeName], MediaStoreName: storeName, TransferOverlap: time.Nanosecond,
	}
}

// placementDeletingFixture は test-store に deleting の原本コピー、archive に overlap を過ぎた desired のコピーを置く。
func placementDeletingFixture(t *testing.T, pool *pgxpool.Pool, roots map[string]string) (assetID int64, sourcePath string) {
	t.Helper()
	relPath := "sites/default/placement/source.ts"
	recordingID := dbtest.Recording(t, pool)
	assetID = seedOriginalAsset(t, pool, roots["test-store"], recordingID, relPath, []byte("placement bytes"))
	archivePath := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, []byte("placement bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	transferTestAddCopy(t, pool, assetID, "archive", relPath)
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'archive'`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET state = 'deleting' WHERE media_asset_id = $1 AND store = 'test-store'`, assetID); err != nil {
		t.Fatal(err)
	}
	return assetID, filepath.Join(roots["test-store"], filepath.FromSlash(relPath))
}

func copyStateIn(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2 ORDER BY id DESC LIMIT 1`, assetID, storeName).Scan(&state); err != nil {
		t.Fatalf("reading %s copy state: %v", storeName, err)
	}
	return state
}

func TestDeleteReconcilePendingPathFinishesPlacementDeleting(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	cfg := transferTestConfig("archive", "archive", "test-store")
	assetID, sourcePath := placementDeletingFixture(t, pool, roots)

	if err := placementDeleteWorker(pool, cfg, roots, "test-store").Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work: %v", err)
	}
	if got := copyStateIn(t, pool, assetID, "test-store"); got != "deleted" {
		t.Errorf("deleting placement copy state after one pass = %q, want deleted", got)
	}
	if fileExists(sourcePath) {
		t.Errorf("file of the deleting placement copy %q remains after the pending path", sourcePath)
	}
}

func TestDeleteReconcileRevertsPlacementDeletingWhenDesiredMissing(t *testing.T) {
	setMissing := func(t *testing.T, pool *pgxpool.Pool, assetID int64) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `
INSERT INTO missing_media_assets (media_asset_copy_id)
SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("desired copy marked missing returns the deleting copy to active", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		roots := transferTestRoots(t, "test-store", "archive")
		cfg := transferTestConfig("archive", "archive", "test-store")
		assetID, sourcePath := placementDeletingFixture(t, pool, roots)
		setMissing(t, pool, assetID)

		if err := placementDeleteWorker(pool, cfg, roots, "test-store").Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
			t.Fatalf("DeleteReconcileWorker.Work: %v", err)
		}
		if got := copyStateIn(t, pool, assetID, "test-store"); got != "active" {
			t.Errorf("deleting copy state after its desired copy was marked missing = %q, want active", got)
		}
		if !fileExists(sourcePath) {
			t.Error("source file was removed although its desired copy is not present")
		}
	})

	t.Run("deleting copy whose desired copy is present is not listed as unqualified", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		roots := transferTestRoots(t, "test-store", "archive")
		cfg := transferTestConfig("archive", "archive", "test-store")
		placementDeletingFixture(t, pool, roots)
		rows, err := pgstore.NewMedia(pool, "test-store", "").ListUnqualifiedDeletingAssets(
			context.Background(), time.Now().Add(-time.Hour), time.Now(), mustOriginalRules(t, cfg), "archive", 10)
		if err != nil || len(rows) != 0 {
			t.Fatalf("unqualified deleting rows = %+v, %v; want none while the desired copy is present", rows, err)
		}
	})

	t.Run("revert re-evaluates when the desired copy recovers after listing", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		roots := transferTestRoots(t, "test-store", "archive")
		cfg := transferTestConfig("archive", "archive", "test-store")
		assetID, _ := placementDeletingFixture(t, pool, roots)
		setMissing(t, pool, assetID)
		media := pgstore.NewMedia(pool, "test-store", "")
		rules := mustOriginalRules(t, cfg)
		grace, overlap := time.Now().Add(-time.Hour), time.Now()

		rows, err := media.ListUnqualifiedDeletingAssets(context.Background(), grace, overlap, rules, "archive", 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("unqualified deleting rows = %+v, %v; want the placement copy", rows, err)
		}
		if _, err := pool.Exec(context.Background(), `DELETE FROM missing_media_assets`); err != nil {
			t.Fatal(err)
		}
		reverted, err := media.RevertToActive(context.Background(), rows[0].ID, grace, overlap, rules, "archive")
		if err != nil {
			t.Fatalf("RevertToActive: %v", err)
		}
		if reverted {
			t.Error("RevertToActive returned true although the desired copy is present again")
		}
		if got := copyStateIn(t, pool, assetID, "test-store"); got != "deleting" {
			t.Errorf("copy state after a revert that no longer applies = %q, want deleting", got)
		}
	})
}

// TestStorageTransferPolicyRoundTripConverges は規則を A→B→A と切り替えても、各段で
// active・実体ありのコピーが 1 つ以上残り、最後は desired の置き場だけに収束することを確かめる。
func TestStorageTransferPolicyRoundTripConverges(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	relPath := "sites/default/roundtrip/source.ts"
	recordingID := dbtest.Recording(t, pool)
	assetID := seedOriginalAsset(t, pool, roots["test-store"], recordingID, relPath, []byte("round trip bytes"))
	media := pgstore.NewMedia(pool, "test-store", "")

	assertPresent := func(t *testing.T, step string) {
		t.Helper()
		rows, err := pool.Query(context.Background(), `SELECT store FROM media_asset_copies WHERE media_asset_id = $1 AND state = 'active'`, assetID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		present := 0
		for rows.Next() {
			var storeName string
			if err := rows.Scan(&storeName); err != nil {
				t.Fatal(err)
			}
			if fileExists(filepath.Join(roots[storeName], filepath.FromSlash(relPath))) {
				present++
			}
		}
		if present == 0 {
			t.Fatalf("%s: no active copy with a file remains", step)
		}
	}
	for i, desired := range []string{"archive", "test-store", "archive"} {
		cfg := transferTestConfig(desired, "archive", "test-store")
		fingerprint, err := cfg.PolicyFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		transfer := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots}
		deleters := []*DeleteReconcileWorker{
			placementDeleteWorker(pool, cfg, roots, "test-store"), placementDeleteWorker(pool, cfg, roots, "archive"),
		}
		for pass := 0; pass < 3; pass++ {
			step := "stage " + desired
			for _, deleter := range deleters {
				if err := deleter.Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
					t.Fatalf("%s pass %d (round %d): delete reconcile: %v", step, pass, i, err)
				}
				assertPresent(t, step+" after delete reconcile")
			}
			if err := transfer.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: desired, PolicyFingerprint: fingerprint}); err != nil {
				t.Fatalf("%s pass %d (round %d): transfer: %v", step, pass, i, err)
			}
			assertPresent(t, step+" after transfer")
		}
		var stores []string
		rows, err := pool.Query(context.Background(), `SELECT store FROM media_asset_copies WHERE media_asset_id = $1 AND state <> 'deleted' ORDER BY store`, assetID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var storeName string
			if err := rows.Scan(&storeName); err != nil {
				t.Fatal(err)
			}
			stores = append(stores, storeName)
		}
		rows.Close()
		if len(stores) != 1 || stores[0] != desired {
			t.Fatalf("round %d: surviving copies = %v, want only %q", i, stores, desired)
		}
		for name, root := range roots {
			if got, want := fileExists(filepath.Join(root, filepath.FromSlash(relPath))), name == desired; got != want {
				t.Fatalf("round %d: file in %s exists = %t, want %t", i, name, got, want)
			}
		}
	}
}

func TestCompleteMediaAssetTransferRejectsChangedSHA(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("sha guarded bytes"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	rules := mustOriginalRules(t, cfg)
	media := pgstore.NewMedia(pool, "test-store", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", rules, cfg.FSStoreNames(), cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("PrepareMediaAssetTransfer = (%+v, %t, %v)", transfer, ok, err)
	}
	recorded := []byte("0123456789abcdef0123456789abcdef")
	observed := []byte("fedcba9876543210fedcba9876543210")
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, recorded, assetID); err != nil {
		t.Fatal(err)
	}

	active, outcome, err := media.CompleteMediaAssetTransfer(context.Background(), transfer, "archive", cfg.Derived, rules, observed)
	if err != nil {
		t.Fatalf("CompleteMediaAssetTransfer: %v", err)
	}
	if active || outcome != "sha_mismatch" {
		t.Errorf("CompleteMediaAssetTransfer = (active=%t, %q), want (false, sha_mismatch)", active, outcome)
	}
	var sha []byte
	if err := pool.QueryRow(context.Background(), `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if string(sha) != string(recorded) {
		t.Errorf("media_assets.sha256 = %x, want the recorded %x kept", sha, recorded)
	}
	if got := copyStateIn(t, pool, assetID, "archive"); got != "pending" {
		t.Errorf("destination copy state = %q, want pending", got)
	}
}

// TestStorageTransferCorruptRetirementSerializesWithUntilEncoded は、破損した派生コピーの退役と
// until_encoded の原本削除が、同じ録画の行ロックで直列化されることを確かめる。
// 派生物の desired の置き場を test-store にして、どちらが先でも他方の資格が消える構成にしてある。
func TestStorageTransferCorruptRetirementSerializesWithUntilEncoded(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store")
	recordingID := dbtest.Recording(t, pool)
	originalID := seedOriginalAsset(t, pool, roots["test-store"], recordingID, "sites/default/serialize/original.ts", []byte("original"))
	profile := "web"
	seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/serialize/encoded.mp4", []byte("encoded"))
	thumbnailID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindThumbnail, nil, "sites/default/serialize/thumbnail.jpg", []byte("thumbnail"))
	seedSeekTilesAsset(t, pool, roots["test-store"], recordingID, "sites/default/serialize/tiles.jpg")
	markRecordingUntilEncoded(t, pool, recordingID, []string{profile})
	var eligible bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM until_encoded_deletable_originals('test-store') WHERE asset_id = $1)`, originalID).Scan(&eligible); err != nil || !eligible {
		t.Fatalf("until_encoded fixture eligibility = %t, %v; want true", eligible, err)
	}
	originalCopyID := transferTestCopyID(t, pool, originalID, "test-store")
	thumbnailCopyID := transferTestCopyID(t, pool, thumbnailID, "test-store")

	if _, err := pool.Exec(context.Background(), `
CREATE FUNCTION public.pause_storage_copy_delete_for_test() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IS DISTINCT FROM NEW.state AND NEW.state = 'deleting' THEN
        PERFORM pg_sleep(0.3);
    END IF;
    RETURN NEW;
END
$$`); err != nil {
		t.Fatalf("creating delete-race trigger function: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
CREATE TRIGGER pause_storage_copy_delete_for_test
BEFORE UPDATE OF state ON media_asset_copies
FOR EACH ROW EXECUTE FUNCTION public.pause_storage_copy_delete_for_test()`); err != nil {
		t.Fatalf("creating delete-race trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS pause_storage_copy_delete_for_test ON media_asset_copies`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS public.pause_storage_copy_delete_for_test()`)
	})

	media := pgstore.NewMedia(pool, "test-store", "test-store")
	start := make(chan struct{})
	results := make(chan struct {
		marked bool
		err    error
	}, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		marked, err := media.MarkMediaAssetCopyCorrupt(context.Background(), recordingID, thumbnailID, thumbnailCopyID, "test-store")
		results <- struct {
			marked bool
			err    error
		}{marked, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		marked, err := media.MarkUntilEncodedDeleting(context.Background(), recordingID, originalID, originalCopyID)
		results <- struct {
			marked bool
			err    error
		}{marked, err}
	}()
	close(start)
	wg.Wait()
	close(results)
	markedCount := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("marking copies deleting: %v", got.err)
		}
		if got.marked {
			markedCount++
		}
	}
	if markedCount != 1 {
		t.Fatalf("copies marked deleting by the corrupt retirement and the until_encoded deletion = %d, want exactly one", markedCount)
	}
}
