package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func transferTestConfig(desiredStore string, stores ...string) config.StorageConfig {
	storeConfigs := make(map[string]config.StoreConfig, len(stores))
	for _, name := range stores {
		storeConfigs[name] = config.StoreConfig{Kind: "fs"}
	}
	return config.StorageConfig{
		Stores:    storeConfigs,
		Originals: []config.OriginalStoreRule{{Store: desiredStore}},
		Derived:   desiredStore,
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
}

func transferTestRoots(t *testing.T, stores ...string) map[string]string {
	t.Helper()
	roots := make(map[string]string, len(stores))
	for _, name := range stores {
		root := t.TempDir()
		if err := mediapath.WriteStoreMarker(root, name); err != nil {
			t.Fatalf("writing %s store marker: %v", name, err)
		}
		roots[name] = root
	}
	return roots
}

func transferTestOriginal(t *testing.T, pool *pgxpool.Pool, sourceRoot string, content []byte) int64 {
	t.Helper()
	recordingID := dbtest.Recording(t, pool)
	return seedOriginalAsset(t, pool, sourceRoot, recordingID, "sites/default/transfer/source.ts", content)
}

func transferTestCopyID(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, storeName).Scan(&id); err != nil {
		t.Fatalf("reading copy ID for asset %d in %s: %v", assetID, storeName, err)
	}
	return id
}

func transferTestAddCopy(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName, relPath string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
VALUES ($1, $2, $3, 'active')
ON CONFLICT (media_asset_id, store) DO UPDATE
SET rel_path = EXCLUDED.rel_path, state = 'active', deleted_at = NULL, created_at = now()`, assetID, storeName, relPath); err != nil {
		t.Fatalf("adding active copy in %s: %v", storeName, err)
	}
}

func TestStorageTransferWorkerCopiesAndAdopts(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		existing                  bool
		resume                    bool
		sourceChanged             bool
		corruptCheckpointedPrefix bool
	}{
		{name: "copy computes and stores SHA-256"},
		{name: "matching restored destination is adopted", existing: true},
		{name: "checkpointed temp resumes", resume: true},
		{name: "changed source prefix rejects checkpoint without deleting original", resume: true, sourceChanged: true},
		{name: "damaged checkpointed temp prefix is recopied", resume: true, corruptCheckpointedPrefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			content := []byte("transfer-asset-bytes")
			assetID := transferTestOriginal(t, pool, roots["test-store"], content)
			targetPath := filepath.Join(roots["archive"], "sites/default/transfer/source.ts")
			sourcePath := filepath.Join(roots["test-store"], "sites/default/transfer/source.ts")
			var changedSource []byte
			if tc.existing {
				if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targetPath, content, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cfg := transferTestConfig("archive", "archive", "test-store")
			fingerprint, err := cfg.PolicyFingerprint()
			if err != nil {
				t.Fatal(err)
			}
			media := pgstore.NewMedia(pool, "test-store")
			if tc.resume {
				transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, 10)
				if err != nil || !ok {
					t.Fatalf("preparing checkpointed transfer = (%+v, %t, %v)", transfer, ok, err)
				}
				tempPath := filepath.Join(roots["archive"], "sites/default/transfer", fmt.Sprintf(".rokuban-transfer-%d-%d", assetID, transfer.CopyID))
				if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
					t.Fatal(err)
				}
				partial := content[:len(content)/2]
				if err := os.WriteFile(tempPath, partial, 0o644); err != nil {
					t.Fatal(err)
				}
				hasher := sha256.New()
				if _, err := hasher.Write(partial); err != nil {
					t.Fatal(err)
				}
				if err := fsink.WriteCheckpoint(tempPath, int64(len(partial)), hasher, nil); err != nil {
					t.Fatalf("writing transfer checkpoint: %v", err)
				}
				if tc.corruptCheckpointedPrefix {
					corrupted := bytes.Clone(partial)
					corrupted[0] ^= 0xff
					if err := os.WriteFile(tempPath, corrupted, 0o644); err != nil {
						t.Fatalf("corrupting checkpointed transfer temp prefix: %v", err)
					}
				}
				if tc.sourceChanged {
					changedSource = bytes.Clone(content)
					changedSource[0] ^= 0xff
					if err := os.WriteFile(sourcePath, changedSource, 0o644); err != nil {
						t.Fatalf("changing source prefix before resume: %v", err)
					}
				}
			}
			worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots}
			if err := worker.Work(context.Background(), jobs.StorageTransferArgs{
				Action: jobs.StorageTransferActionCopy, MediaAssetID: assetID,
				Store: "archive", PolicyFingerprint: fingerprint,
			}); err != nil {
				t.Fatalf("StorageTransferWorker.Work: %v", err)
			}
			if tc.sourceChanged {
				if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("destination stat error = %v, want unpublished destination", err)
				}
				gotSource, err := os.ReadFile(sourcePath)
				if err != nil || !bytes.Equal(gotSource, changedSource) {
					t.Fatalf("source after mismatch = %q, %v; want preserved %q", gotSource, err, changedSource)
				}
				var state string
				var attemptCount int32
				if err := pool.QueryRow(context.Background(), `
SELECT c.state, a.attempt_count
FROM media_asset_copies c
JOIN media_asset_transfer_attempts a ON a.media_asset_id = c.media_asset_id AND a.store = c.store
WHERE c.media_asset_id = $1 AND c.store = 'archive'`, assetID).Scan(&state, &attemptCount); err != nil {
					t.Fatalf("reading failed transfer state: %v", err)
				}
				if state != "pending" || attemptCount != 1 {
					t.Fatalf("copy/failure state = %q/%d, want pending/1", state, attemptCount)
				}
				return
			}
			got, err := os.ReadFile(targetPath)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("destination bytes = %q, %v; want %q", got, err, content)
			}
			expectedSHA := sha256.Sum256(content)
			var assetSHA []byte
			var copyState string
			if err := pool.QueryRow(context.Background(), `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&assetSHA); err != nil {
				t.Fatalf("reading observed SHA-256: %v", err)
			}
			if !bytes.Equal(assetSHA, expectedSHA[:]) {
				t.Fatalf("media_assets.sha256 = %x, want %x", assetSHA, expectedSHA)
			}
			if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copyState); err != nil {
				t.Fatalf("reading destination copy state: %v", err)
			}
			if copyState != "active" {
				t.Fatalf("destination copy state = %q, want active", copyState)
			}
		})
	}
}

func TestStorageTransferWorkerReportsOriginalMismatchWithoutDeletingSource(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	actual := []byte("badbytes")
	assetID := transferTestOriginal(t, pool, roots["test-store"], actual)
	expected := sha256.Sum256([]byte("goodbyte"))
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, expected[:], assetID); err != nil {
		t.Fatalf("setting expected source checksum: %v", err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store"), Config: cfg, Roots: roots}
	if err := worker.Work(context.Background(), jobs.StorageTransferArgs{
		Action: jobs.StorageTransferActionCopy, MediaAssetID: assetID,
		Store: "archive", PolicyFingerprint: fingerprint,
	}); err != nil {
		t.Fatalf("Work mismatch: %v", err)
	}
	if got := assetState(t, pool, assetID); got != "active" {
		t.Fatalf("original state = %q, want active", got)
	}
	if got, err := os.ReadFile(filepath.Join(roots["test-store"], "sites/default/transfer/source.ts")); err != nil || !bytes.Equal(got, actual) {
		t.Fatalf("source after mismatch = %q, %v; want %q", got, err, actual)
	}
	if fileExists(filepath.Join(roots["archive"], "sites/default/transfer/source.ts")) {
		t.Fatal("mismatched source was published to destination")
	}
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT attempt_count FROM media_asset_transfer_attempts WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&attempts); err != nil {
		t.Fatalf("reading failed attempt: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("transfer attempt count = %d, want 1", attempts)
	}
}

func TestStorageTransferWorkerDeletesDerivedAssetAfterItsOnlyCopyIsCorrupt(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	relPath := "sites/default/transfer/thumbnail.jpg"
	content := []byte("thumbnail bytes")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindThumbnail, nil, relPath, content)
	wrongSHA := sha256.Sum256([]byte("different thumbnail bytes"))
	if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET sha256 = $1 WHERE id = $2", wrongSHA[:], assetID); err != nil {
		t.Fatalf("setting mismatched logical checksum: %v", err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store"), Config: cfg, Roots: roots}
	if err := worker.Work(context.Background(), jobs.StorageTransferArgs{
		Action: jobs.StorageTransferActionCopy, MediaAssetID: assetID,
		Store: "archive", PolicyFingerprint: fingerprint,
	}); err != nil {
		t.Fatalf("Work with corrupt sole derived copy: %v", err)
	}

	var assetState, sourceState, targetState string
	if err := pool.QueryRow(context.Background(),
		"SELECT a.state, source.state, target.state FROM media_assets a JOIN media_asset_copies source ON source.media_asset_id = a.id AND source.store = $2 JOIN media_asset_copies target ON target.media_asset_id = a.id AND target.store = $3 WHERE a.id = $1",
		assetID, "test-store", "archive").Scan(&assetState, &sourceState, &targetState); err != nil {
		t.Fatalf("reading asset/copy states after derived source mismatch: %v", err)
	}
	if assetState != "deleted" || sourceState != "deleted" || targetState != "deleted" {
		t.Fatalf("asset/source/target states = %q/%q/%q, want deleted/deleted/deleted", assetState, sourceState, targetState)
	}
	if fileExists(filepath.Join(roots["archive"], filepath.FromSlash(relPath))) {
		t.Fatal("mismatched derived source was published to the destination")
	}
}

func TestStorageTransferWorkerDoesNotInvalidateValidDerivedSourceForDamagedTemp(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	relPath := "sites/default/transfer/thumbnail.jpg"
	content := []byte("thumbnail bytes that survive a damaged temporary transfer")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindThumbnail, nil, relPath, content)
	expectedSHA := sha256.Sum256(content)
	if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET sha256 = $1 WHERE id = $2", expectedSHA[:], assetID); err != nil {
		t.Fatalf("setting logical checksum: %v", err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	rules := mustOriginalRules(t, cfg)
	media := pgstore.NewMedia(pool, "test-store")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", rules, cfg.FSStoreNames(), cfg.Derived, 10)
	if err != nil || !ok {
		t.Fatalf("preparing damaged resumed transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	tempPath := filepath.Join(roots["archive"], filepath.Dir(filepath.FromSlash(relPath)), fmt.Sprintf(".rokuban-transfer-%d-%d", assetID, transfer.CopyID))
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatal(err)
	}
	checkpointedPrefix := content[:len(content)/2]
	damagedTemp := append(bytes.Clone(checkpointedPrefix), bytes.Clone(content[len(checkpointedPrefix):])...)
	damagedTemp[len(checkpointedPrefix)] ^= 0xff
	if err := os.WriteFile(tempPath, damagedTemp, 0o644); err != nil {
		t.Fatalf("writing damaged transfer temp: %v", err)
	}
	checkpointHasher := sha256.New()
	if _, err := checkpointHasher.Write(checkpointedPrefix); err != nil {
		t.Fatal(err)
	}
	if err := fsink.WriteCheckpoint(tempPath, int64(len(checkpointedPrefix)), checkpointHasher, nil); err != nil {
		t.Fatalf("writing transfer checkpoint: %v", err)
	}

	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots}
	args := jobs.StorageTransferArgs{Action: jobs.StorageTransferActionCopy, MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}
	if err := worker.Work(context.Background(), args); err != nil {
		t.Fatalf("Work with damaged uncheckpointed temp tail: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Fatalf("derived asset state after damaged temp = %q, want active", got)
	}
	var sourceState, targetState string
	var attemptCount int32
	if err := pool.QueryRow(context.Background(),
		"SELECT source.state, target.state, attempts.attempt_count FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id JOIN media_asset_transfer_attempts attempts ON attempts.media_asset_id = target.media_asset_id AND attempts.store = target.store WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
		assetID, "test-store", "archive").Scan(&sourceState, &targetState, &attemptCount); err != nil {
		t.Fatalf("reading copy/failure state after damaged temp: %v", err)
	}
	if sourceState != "active" || targetState != "pending" || attemptCount != 1 {
		t.Fatalf("source/target/failure states = %q/%q/%d, want active/pending/1", sourceState, targetState, attemptCount)
	}
	if fileExists(filepath.Join(roots["archive"], filepath.FromSlash(relPath))) || fileExists(tempPath) {
		t.Fatal("damaged transfer temp or destination remains after rejection")
	}

	if err := worker.Work(context.Background(), args); err != nil {
		t.Fatalf("retrying transfer after damaged temp was discarded: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(roots["archive"], filepath.FromSlash(relPath)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("recovered destination bytes = %q, %v; want %q", got, err, content)
	}
	if got := assetState(t, pool, assetID); got != "active" {
		t.Fatalf("derived asset state after retry = %q, want active", got)
	}
	if err := pool.QueryRow(context.Background(),
		"SELECT source.state, target.state FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
		assetID, "test-store", "archive").Scan(&sourceState, &targetState); err != nil {
		t.Fatalf("reading copy states after recovery: %v", err)
	}
	if sourceState != "active" || targetState != "active" {
		t.Fatalf("copy states after retry = %q/%q, want active/active", sourceState, targetState)
	}
}

func TestStorageTransferWorkerRejectsOldFingerprintAndNewPolicyKeepsAValidCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive", "cold")
	content := []byte("asset after policy change")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	media := pgstore.NewMedia(pool, "test-store")
	oldConfig := transferTestConfig("archive", "archive", "cold", "test-store")
	oldFingerprint, err := oldConfig.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	newConfig := transferTestConfig("cold", "archive", "cold", "test-store")
	newFingerprint, err := newConfig.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	oldJob := jobs.StorageTransferArgs{Action: jobs.StorageTransferActionCopy, MediaAssetID: assetID, Store: "archive", PolicyFingerprint: oldFingerprint}
	worker := StorageTransferWorker{Storage: media, Config: newConfig, Roots: roots}
	if err := worker.Work(context.Background(), oldJob); !errors.Is(err, jobq.ErrDiscard) {
		t.Fatalf("old policy job error = %v, want jobq.ErrDiscard", err)
	}
	var archiveCopies int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&archiveCopies); err != nil {
		t.Fatal(err)
	}
	if archiveCopies != 0 {
		t.Fatalf("old policy created %d archive copies, want 0", archiveCopies)
	}
	if err := worker.Work(context.Background(), jobs.StorageTransferArgs{
		Action: jobs.StorageTransferActionCopy, MediaAssetID: assetID, Store: "cold", PolicyFingerprint: newFingerprint,
	}); err != nil {
		t.Fatalf("copying under new policy: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'cold'`, assetID); err != nil {
		t.Fatal(err)
	}
	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), mustOriginalRules(t, newConfig), "cold", newConfig.Transfer.Overlap.Microseconds(), 10)
	if err != nil {
		t.Fatalf("listing stale copies after policy switch: %v", err)
	}
	var sourceCandidate *store.MediaAssetCopyDeleteCandidate
	for i := range candidates {
		if candidates[i].Store == "test-store" {
			sourceCandidate = &candidates[i]
			break
		}
	}
	if sourceCandidate == nil {
		t.Fatalf("stale source copy is not eligible after the new desired copy is active: %+v", candidates)
	}
	deletionWorker := StorageTransferWorker{Storage: media, Config: newConfig, Roots: roots}
	if err := deletionWorker.Work(context.Background(), jobs.StorageTransferArgs{
		Action: jobs.StorageTransferActionDelete, MediaAssetID: assetID,
		CopyID: sourceCandidate.CopyID, Store: "test-store", PolicyFingerprint: newFingerprint,
	}); err != nil {
		t.Fatalf("deleting stale source copy: %v", err)
	}
	if fileExists(filepath.Join(roots["test-store"], "sites/default/transfer/source.ts")) {
		t.Fatal("stale source file remains after delete job")
	}
	var sourceState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'test-store'`, assetID).Scan(&sourceState); err != nil {
		t.Fatal(err)
	}
	if sourceState != "deleted" {
		t.Fatalf("stale source copy state = %q, want deleted", sourceState)
	}
	var activeStores []string
	rows, err := pool.Query(context.Background(), `SELECT store FROM media_asset_copies WHERE media_asset_id = $1 AND state = 'active' ORDER BY store`, assetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		activeStores = append(activeStores, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(activeStores, []string{"cold"}) {
		t.Fatalf("active stores after old-copy deletion mark = %v, want [cold]", activeStores)
	}
}

func TestStorageTransferWorkerReconcileEnqueuesDesiredCopies(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("pending transfer"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	enqueuer := newRecordingEnqueuer()
	reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMedia(pool, "test-store"), Config: cfg}
	if err := reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}); err != nil {
		t.Fatalf("reconcile Work: %v", err)
	}
	args := enqueuer.Enqueued("storage_transfer")
	if len(args) != 1 {
		t.Fatalf("enqueued storage_transfer jobs = %d, want 1", len(args))
	}
	got, ok := args[0].(jobs.StorageTransferArgs)
	if !ok {
		t.Fatalf("enqueued arg type = %T, want jobs.StorageTransferArgs", args[0])
	}
	if got.Action != jobs.StorageTransferActionCopy || got.MediaAssetID != assetID || got.Store != "archive" || got.PolicyFingerprint != fingerprint {
		t.Fatalf("enqueued args = %+v, want copy for asset %d to archive with current fingerprint", got, assetID)
	}
	if queue, err := jobs.PhysicalQueueFor(got); err != nil || queue != "transfer_archive" {
		t.Fatalf("PhysicalQueueFor = %q, %v; want transfer_archive", queue, err)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferWaitingAssets); got != 1 {
		t.Fatalf("waiting assets gauge = %v, want 1", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferWaitingBytes); got != float64(len("pending transfer")) {
		t.Fatalf("waiting bytes gauge = %v, want %d", got, len("pending transfer"))
	}
}

func TestUntilEncodedOriginalWaitsForPresentDerivedCopiesInDesiredStore(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "sites/default/retention/original.ts", []byte("original"))
	profile := "h264"
	encodedID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, "sites/default/retention/encoded.mp4", []byte("encoded"))
	thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, "sites/default/retention/thumbnail.jpg", []byte("thumb"))
	seekTilesID := seedSeekTilesAsset(t, pool, mediaDir, recordingID, "sites/default/retention/tiles.jpg")
	markRecordingUntilEncoded(t, pool, recordingID, []string{profile})

	assertOriginalEligibility := func(want bool) {
		t.Helper()
		var count int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM until_encoded_deletable_originals('archive') WHERE id = $1`, originalID).Scan(&count); err != nil {
			t.Fatalf("querying desired-store retention predicate: %v", err)
		}
		if got := count == 1; got != want {
			t.Fatalf("until_encoded eligibility = %t, want %t", got, want)
		}
	}
	assertOriginalEligibility(false)
	transferTestAddCopy(t, pool, encodedID, "archive", "sites/default/retention/encoded.mp4")
	transferTestAddCopy(t, pool, thumbnailID, "archive", "sites/default/retention/thumbnail.jpg")
	assertOriginalEligibility(false)
	transferTestAddCopy(t, pool, seekTilesID, "archive", "sites/default/retention/tiles.jpg")
	assertOriginalEligibility(true)
	if _, err := pool.Exec(context.Background(), `INSERT INTO missing_media_assets (media_asset_id, store) VALUES ($1, 'archive')`, seekTilesID); err != nil {
		t.Fatalf("marking desired seek tiles copy missing: %v", err)
	}
	assertOriginalEligibility(false)
	if _, err := pool.Exec(context.Background(), `DELETE FROM missing_media_assets WHERE media_asset_id = $1 AND store = 'archive'`, seekTilesID); err != nil {
		t.Fatal(err)
	}
	assertOriginalEligibility(true)

	cfg := transferTestConfig("archive", "archive", "test-store")
	candidates, err := pgstore.NewMedia(pool, "test-store").ListMediaAssetTransferCandidates(
		context.Background(), mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, 10, 100,
	)
	if err != nil {
		t.Fatalf("listing transfer candidates after until_encoded eligibility: %v", err)
	}
	for _, candidate := range candidates {
		if candidate.MediaAssetID == originalID {
			t.Fatal("until_encoded original remained a transfer candidate after the desired derived copies were present")
		}
	}
}

func TestStorageTransferDeletionRequiresDesiredPresentCopyAndOverlap(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/delete/source.ts"
	})
	cfg := transferTestConfig("archive", "archive", "test-store")
	rules := mustOriginalRules(t, cfg)
	media := pgstore.NewMedia(pool, "test-store")
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "archive", 0, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates without a desired copy = %+v, %v; want none", candidates, err)
	}

	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/delete/source.ts")
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "archive", int64(time.Hour/time.Microsecond), 10); err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates before overlap = %+v, %v; want none", candidates, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET created_at = now() - interval '2 hours' WHERE media_asset_id = $1 AND store = 'archive'`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO missing_media_assets (media_asset_id, store) VALUES ($1, 'archive')`, assetID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "archive", 0, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates with desired copy marked missing = %+v, %v; want none", candidates, err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM missing_media_assets WHERE media_asset_id = $1 AND store = 'archive'`, assetID); err != nil {
		t.Fatal(err)
	}
	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "archive", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Store != "test-store" {
		t.Fatalf("stale copy candidates = %+v, want only test-store", candidates)
	}
	path, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), candidates[0], rules, "archive", 0)
	if err != nil || !marked || path != "sites/default/delete/source.ts" {
		t.Fatalf("marking stale copy = (%q, %t, %v), want path, true, nil", path, marked, err)
	}
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "archive", 0, 10); err != nil || len(candidates) != 1 || candidates[0].Store != "test-store" {
		t.Fatalf("deletion candidates after stale copy marked = %+v, %v; want deleting test-store copy for retry", candidates, err)
	}
}

func TestStorageTransferDeletionSerializesPolicySwitches(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/policy-switch/source.ts"
	})
	oldConfig := transferTestConfig("archive", "archive", "cold", "test-store")
	newConfig := transferTestConfig("cold", "archive", "cold", "test-store")
	oldRules := mustOriginalRules(t, oldConfig)
	newRules := mustOriginalRules(t, newConfig)
	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/policy-switch/source.ts")
	transferTestAddCopy(t, pool, assetID, "cold", "sites/default/policy-switch/source.ts")
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = 'test-store'`, assetID); err != nil {
		t.Fatal(err)
	}
	archive := store.MediaAssetCopyDeleteCandidate{CopyID: transferTestCopyID(t, pool, assetID, "archive"), MediaAssetID: assetID, Store: "archive", RelPath: "sites/default/policy-switch/source.ts"}
	cold := store.MediaAssetCopyDeleteCandidate{CopyID: transferTestCopyID(t, pool, assetID, "cold"), MediaAssetID: assetID, Store: "cold", RelPath: "sites/default/policy-switch/source.ts"}

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

	media := pgstore.NewMedia(pool, "test-store")
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
		_, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), cold, oldRules, "archive", 0)
		results <- struct {
			marked bool
			err    error
		}{marked, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		_, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), archive, newRules, "cold", 0)
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
			t.Fatalf("marking copies during policy switch: %v", got.err)
		}
		if got.marked {
			markedCount++
		}
	}
	if markedCount != 1 {
		t.Fatalf("deletion marks across old/new policies = %d, want exactly one", markedCount)
	}
	var activeCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND state = 'active'`, assetID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active copies after concurrent policy switch = %d, want 1", activeCount)
	}
}

func mustOriginalRules(t *testing.T, cfg config.StorageConfig) []byte {
	t.Helper()
	rules, err := cfg.OriginalRulesJSON()
	if err != nil {
		t.Fatal(err)
	}
	return rules
}
