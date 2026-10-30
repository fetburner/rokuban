package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
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
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT a.id, $2, $3, COALESCE(a.derived_generation, 0), 'active'
FROM media_assets a WHERE a.id = $1
ON CONFLICT (media_asset_id, store, generation) WHERE state <> 'deleted' DO UPDATE
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
			media := pgstore.NewMedia(pool, "test-store", "")
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
				MediaAssetID: assetID,
				Store:        "archive", PolicyFingerprint: fingerprint,
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
JOIN media_asset_transfer_attempts a ON a.copy_id = c.id
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

func TestStorageTransferWorkerKeepsEncodedSubtitleSidecarInSync(t *testing.T) {
	for _, tc := range []struct {
		name           string
		existingVideo  bool
		sourceSubtitle bool
		targetSubtitle bool
		wantTargetVTT  bool
	}{
		{name: "new destination copies sidecar", sourceSubtitle: true, wantTargetVTT: true},
		{name: "adopted destination replaces stale sidecar", existingVideo: true, sourceSubtitle: true, targetSubtitle: true, wantTargetVTT: true},
		{name: "destination without source sidecar removes stale sidecar", existingVideo: true, targetSubtitle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			recordingID := dbtest.Recording(t, pool)
			profile := "web"
			relPath := "sites/default/transfer/captions.mp4"
			video := []byte("encoded-video")
			assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, video)
			subtitleRelPath, err := mediapath.SubtitleSibling(relPath)
			if err != nil {
				t.Fatal(err)
			}
			sourceSubtitle := filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath))
			if tc.sourceSubtitle {
				if err := os.WriteFile(sourceSubtitle, []byte("WEBVTT\n\nsource captions\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			targetVideo := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
			if tc.existingVideo || tc.targetSubtitle {
				if err := os.MkdirAll(filepath.Dir(targetVideo), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.existingVideo {
				if err := os.WriteFile(targetVideo, video, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			targetSubtitle := filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath))
			if tc.targetSubtitle {
				if err := os.WriteFile(targetSubtitle, []byte("WEBVTT\n\nstale captions\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cfg := transferTestConfig("archive", "archive", "test-store")
			fingerprint, err := cfg.PolicyFingerprint()
			if err != nil {
				t.Fatal(err)
			}
			worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots}
			if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err != nil {
				t.Fatalf("StorageTransferWorker.Work: %v", err)
			}
			gotSubtitle, err := os.ReadFile(targetSubtitle)
			if tc.wantTargetVTT {
				if err != nil || string(gotSubtitle) != "WEBVTT\n\nsource captions\n" {
					t.Fatalf("destination subtitle = %q, %v; want source sidecar", gotSubtitle, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination subtitle read error = %v, want sidecar removed", err)
			}
			var state string
			if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "active" {
				t.Fatalf("destination copy state = %q, want active", state)
			}
		})
	}
}

func TestStorageTransferWorkerPreservesSubtitleAfterConcurrentActivation(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/concurrent-captions.mp4"
	video := []byte("encoded-video")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, video)
	subtitleRelPath, err := mediapath.SubtitleSibling(relPath)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(roots["test-store"], filepath.FromSlash(relPath))
	sourceSubtitle := filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath))
	if err := os.WriteFile(sourceSubtitle, []byte("WEBVTT\n\nsource captions\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgA := transferTestConfig("archive", "archive", "test-store")
	cfgB := transferTestConfig("archive", "archive", "test-store")
	cfgB.Stores["archive"] = config.StoreConfig{Kind: "fs", InternalURL: "http://archive-v2.internal:8080"}
	fingerprintA, err := cfgA.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	fingerprintB, err := cfgB.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintA == fingerprintB {
		t.Fatal("rollout configs have the same policy fingerprint")
	}
	rulesB := mustOriginalRules(t, cfgB)
	media := pgstore.NewMedia(pool, "test-store", "")
	transferB, ok, err := media.PrepareMediaAssetTransfer(
		context.Background(), assetID, "archive", rulesB, cfgB.FSStoreNames(), cfgB.Derived, storageTransferAttemptLimit,
	)
	if err != nil || !ok {
		t.Fatalf("preparing delayed transfer = (%+v, %t, %v)", transferB, ok, err)
	}

	workerA := StorageTransferWorker{Storage: media, Config: cfgA, Roots: roots}
	if err := workerA.Work(context.Background(), jobs.StorageTransferArgs{
		MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprintA,
	}); err != nil {
		t.Fatalf("first rollout transfer: %v", err)
	}
	targetPath := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
	targetSubtitle := filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath))
	workerB := StorageTransferWorker{Storage: media, Config: cfgB, Roots: roots}
	digest := sha256.Sum256(video)
	if err := workerB.completeTransfer(
		context.Background(), transferB,
		jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprintB},
		rulesB, digest[:], sourcePath, targetPath, "", false, slog.Default(),
	); err != nil {
		t.Fatalf("delayed rollout completion: %v", err)
	}

	gotSubtitle, err := os.ReadFile(targetSubtitle)
	if err != nil || string(gotSubtitle) != "WEBVTT\n\nsource captions\n" {
		t.Fatalf("active destination subtitle = %q, %v; want preserved captions", gotSubtitle, err)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transferB.CopyID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Fatalf("destination state after stale completion = %q, want active", state)
	}
}

func TestDeleteReconcilePlacementDeletionRemovesEncodedSubtitleSidecar(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/captions.mp4"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, []byte("encoded-video"))
	subtitleRelPath, err := mediapath.SubtitleSibling(relPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\nsource captions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(roots["archive"], filepath.FromSlash(relPath))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots["archive"], filepath.FromSlash(relPath)), []byte("encoded-video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\nsource captions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	transferTestAddCopy(t, pool, assetID, "archive", relPath)
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'archive'`, assetID); err != nil {
		t.Fatal(err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMedia(pool, "test-store", "")
	worker := DeleteReconcileWorker{
		MediaDelete: media, Storage: media, StorageTransfer: media, StorageTransferPolicy: cfg,
		RecordingPurge: pgstore.NewRecordings(pool, "test-store"), Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["test-store"], MediaStoreName: "test-store", TransferOverlap: time.Hour,
	}
	if err := worker.Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work: %v", err)
	}
	for _, path := range []string{filepath.Join(roots["test-store"], filepath.FromSlash(relPath)), filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath))} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retired placement path %q stat error = %v, want not-exist", path, err)
		}
	}
}

func TestDeleteReconcileReclaimsObsoletePendingFSCopyAndAllowsPurge(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/abandoned.mp4"
	video := []byte("encoded-video")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, video)
	subtitleRelPath, err := mediapath.SubtitleSibling(relPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\nabandoned captions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMedia(pool, "archive", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing abandoned copy = (%+v, %t, %v)", transfer, ok, err)
	}
	if err := media.RecordMediaAssetTransferFailure(context.Background(), transfer.CopyID, "destination interrupted"); err != nil {
		t.Fatalf("recording failed transfer attempt: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(roots["archive"], filepath.FromSlash(relPath))), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{
		filepath.Join(roots["archive"], filepath.FromSlash(relPath)):         []byte("partially published video"),
		filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath)): []byte("abandoned captions"),
		filepath.Join(roots["archive"], filepath.Dir(filepath.FromSlash(relPath)), fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, assetID, transfer.CopyID)): []byte("partial transfer"),
	} {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	transferTemp := filepath.Join(roots["archive"], filepath.Dir(filepath.FromSlash(relPath)), fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, assetID, transfer.CopyID))
	if err := fsink.WriteCheckpoint(transferTemp, int64(len("partial transfer")), sha256.New(), nil); err != nil {
		t.Fatalf("writing abandoned transfer checkpoint: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = 'test-store'`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() - interval '2 hours' WHERE id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	worker := DeleteReconcileWorker{
		MediaDelete: media, Storage: media, StorageTransfer: media, StorageTransferPolicy: cfg,
		RecordingPurge: pgstore.NewRecordings(pool, "archive"), Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["archive"], MediaStoreName: "archive", TrashRetention: time.Hour,
		OrphanAge: time.Hour, OrphanMTimeGrace: time.Minute,
	}
	deletedBefore := promtestutil.ToFloat64(metrics.DeleteReconcileDeleted.WithLabelValues("pending_transfer_reclaim"))
	if err := worker.Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.DeleteReconcileDeleted.WithLabelValues("pending_transfer_reclaim")); got != deletedBefore+1 {
		t.Errorf("pending transfer reclaim metric = %v, want one increment from %v", got, deletedBefore)
	}
	for _, path := range []string{
		filepath.Join(roots["archive"], filepath.FromSlash(relPath)),
		filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath)),
		transferTemp,
		fsink.CheckpointPath(transferTemp),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("reclaimed pending path %q stat error = %v, want not-exist", path, err)
		}
	}
	var copyState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&copyState); err != nil {
		t.Fatal(err)
	}
	if copyState != "deleted" {
		t.Errorf("abandoned pending copy state = %q, want deleted", copyState)
	}
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts WHERE copy_id = $1`, transfer.CopyID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("retired pending copy attempts = %d, want 0", attempts)
	}
	var purged bool
	if err := pool.QueryRow(context.Background(), `SELECT purged_at IS NOT NULL FROM recordings WHERE id = $1`, recordingID).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if !purged {
		t.Fatal("recording remains unpurged after its last pending FS copy was reclaimed")
	}
}

func TestStorageTransferWorkerResumesKnownSHAPrefixWithoutReadingSourcePrefix(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	content := []byte("known SHA-256 makes the checkpointed destination prefix authoritative")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	expectedSHA := sha256.Sum256(content)
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, expectedSHA[:], assetID); err != nil {
		t.Fatal(err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMedia(pool, "test-store", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, 10)
	if err != nil || !ok {
		t.Fatalf("preparing checkpointed transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	tempPath := filepath.Join(roots["archive"], filepath.Dir(filepath.FromSlash(transfer.RelPath)), fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, assetID, transfer.CopyID))
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prefixSize := len(content) / 2
	prefix := content[:prefixSize]
	if err := os.WriteFile(tempPath, prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	hasher := sha256.New()
	if _, err := hasher.Write(prefix); err != nil {
		t.Fatal(err)
	}
	if err := fsink.WriteCheckpoint(tempPath, int64(prefixSize), hasher, nil); err != nil {
		t.Fatal(err)
	}
	changedSource := bytes.Clone(content)
	changedSource[0] ^= 0xff
	sourcePath := filepath.Join(roots["test-store"], filepath.FromSlash(transfer.SourceRelPath))
	if err := os.WriteFile(sourcePath, changedSource, 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots}
	if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err != nil {
		t.Fatalf("resuming known-SHA transfer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("resumed destination = %q, %v; want checkpoint prefix plus original source suffix %q", got, err, content)
	}
}

func TestStorageTransferWorkerChecksMountsBeforePrepare(t *testing.T) {
	t.Run("missing target does not reserve or count an attempt", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		roots := transferTestRoots(t, "test-store")
		metrics.StorageTransferMountMismatches.DeleteLabelValues("archive")
		t.Cleanup(func() { metrics.StorageTransferMountMismatches.DeleteLabelValues("archive") })
		assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
		cfg := transferTestConfig("archive", "archive", "test-store")
		fingerprint, err := cfg.PolicyFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots}
		if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err == nil {
			t.Fatal("Work with unmounted target succeeded")
		}
		var copies, attempts int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copies); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if copies != 0 || attempts != 0 {
			t.Fatalf("copies/attempts after missing target = %d/%d, want 0/0", copies, attempts)
		}
		if got := promtestutil.ToFloat64(metrics.StorageTransferMountMismatches.WithLabelValues("archive")); got != 1 {
			t.Fatalf("mount mismatch counter = %v, want 1", got)
		}
	})

	t.Run("wrong target marker does not reserve or count an attempt", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		roots := transferTestRoots(t, "test-store", "archive")
		if err := os.WriteFile(filepath.Join(roots["archive"], mediapath.StoreMarkerFileName), []byte("wrong\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		metrics.StorageTransferMountMismatches.DeleteLabelValues("archive")
		t.Cleanup(func() { metrics.StorageTransferMountMismatches.DeleteLabelValues("archive") })
		assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
		cfg := transferTestConfig("archive", "archive", "test-store")
		fingerprint, err := cfg.PolicyFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots}
		if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err == nil {
			t.Fatal("Work with mismatched target marker succeeded")
		}
		var copies, attempts int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copies); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if copies != 0 || attempts != 0 {
			t.Fatalf("copies/attempts after mismatched target marker = %d/%d, want 0/0", copies, attempts)
		}
		if got := promtestutil.ToFloat64(metrics.StorageTransferMountMismatches.WithLabelValues("archive")); got != 1 {
			t.Fatalf("mount mismatch counter = %v, want 1", got)
		}
	})

	t.Run("missing source does not reserve or count an attempt", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		roots := transferTestRoots(t, "test-store", "archive")
		assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
		cfg := transferTestConfig("archive", "archive", "test-store")
		fingerprint, err := cfg.PolicyFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: map[string]string{"archive": roots["archive"]}}
		if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err != nil {
			t.Fatalf("Work with unmounted source: %v", err)
		}
		var copies, attempts int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copies); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if copies != 0 || attempts != 0 {
			t.Fatalf("copies/attempts after missing source = %d/%d, want 0/0", copies, attempts)
		}
	})
}

func TestStorageTransferWorkerSnoozesWhenSourceLockIsBusyWithoutAttempt(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	media := pgstore.NewMedia(pool, "test-store", "")
	lease, acquired, err := media.AcquireTransferSourceBandwidth(context.Background(), "test-store")
	if err != nil || !acquired {
		t.Fatalf("acquiring source lock = (%v, %v), want lease", acquired, err)
	}
	t.Cleanup(func() {
		if err := lease.Release(context.Background()); err != nil {
			t.Errorf("releasing source lock: %v", err)
		}
	})
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots}
	err = worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint})
	if delay, ok := jobq.SnoozeDelay(err); !ok || delay != storageTransferLockSnooze {
		t.Fatalf("Work snooze = (%s, %v) with error %v, want %s", delay, ok, err, storageTransferLockSnooze)
	}
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("transfer attempts while source lock is busy = %d, want 0", attempts)
	}
}

func TestPrepareMediaAssetTransferDoesNotReviveDeletingCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
	if _, err := pool.Exec(context.Background(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, 'archive', 'sites/default/transfer/source.ts', 0, 'deleting')`, assetID); err != nil {
		t.Fatalf("seeding deleting destination: %v", err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	transfer, ok, err := pgstore.NewMedia(pool, "test-store", "").PrepareMediaAssetTransfer(
		context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, 10,
	)
	if err != nil || ok {
		t.Fatalf("preparing while destination is deleting = (%+v, %t, %v), want no reservation", transfer, ok, err)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "deleting" {
		t.Fatalf("destination state after prepare = %q, want deleting", state)
	}
}

func TestTransferTempOrphanCleanupWaitsForTransferLock(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "archive")
	relPath := "sites/default/transfer/.rokuban-transfer-12-34"
	tempPath := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatal(err)
	}
	tempLock, created, err := fsink.LockTempFile(tempPath)
	if err != nil || !created {
		t.Fatalf("creating transfer temp lock = (%v, %v), want new temp", created, err)
	}
	if _, err := tempLock.Write([]byte("partial transfer")); err != nil {
		t.Fatal(err)
	}
	hasher := sha256.New()
	if _, err := hasher.Write([]byte("partial transfer")); err != nil {
		t.Fatal(err)
	}
	if err := fsink.WriteCheckpoint(tempPath, int64(len("partial transfer")), hasher, nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for i, path := range []string{tempPath, fsink.CheckpointPath(tempPath)} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		orphanRelPath := relPath
		if i == 1 {
			orphanRelPath += fsink.CheckpointSuffix
		}
		if _, err := pool.Exec(context.Background(), `INSERT INTO orphan_files (store, rel_path) VALUES ('archive', $1) ON CONFLICT DO NOTHING`, orphanRelPath); err != nil {
			t.Fatal(err)
		}
	}
	worker := DeleteReconcileWorker{
		Storage: pgstore.NewMedia(pool, "archive", ""), MediaDir: roots["archive"], MediaStoreName: "archive",
	}
	worker.deleteOrphanFile(relPath, time.Hour)
	if !fileExists(tempPath) || !fileExists(fsink.CheckpointPath(tempPath)) {
		t.Fatal("active transfer temp or checkpoint was removed while locked")
	}
	if err := tempLock.Close(); err != nil {
		t.Fatal(err)
	}
	worker.deleteOrphanFile(relPath, time.Hour)
	if fileExists(tempPath) || fileExists(fsink.CheckpointPath(tempPath)) {
		t.Fatal("aged transfer temp or checkpoint remains after the lock was released")
	}
	var orphanCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM orphan_files WHERE store = 'archive'`).Scan(&orphanCount); err != nil {
		t.Fatal(err)
	}
	if orphanCount != 0 {
		t.Fatalf("orphan rows after transfer temp cleanup = %d, want 0", orphanCount)
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
	worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots}
	if err := worker.Work(context.Background(), jobs.StorageTransferArgs{
		MediaAssetID: assetID,
		Store:        "archive", PolicyFingerprint: fingerprint,
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
	if err := pool.QueryRow(context.Background(), `SELECT attempt.attempt_count FROM media_asset_transfer_attempts attempt JOIN media_asset_copies copy ON copy.id = attempt.copy_id WHERE copy.media_asset_id = $1 AND copy.store = 'archive'`, assetID).Scan(&attempts); err != nil {
		t.Fatalf("reading failed attempt: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("transfer attempt count = %d, want 1", attempts)
	}
}

func TestStorageTransferWorkerRetiresCorruptDerivedSourceOnlyWhenOriginalCanRebuild(t *testing.T) {
	for _, tc := range []struct {
		name            string
		originalPresent bool
		wantSource      string
		wantTarget      string
		wantAttempts    int
	}{
		{name: "present original permits rebuild", originalPresent: true, wantSource: "deleted", wantTarget: "deleted"},
		{name: "missing original reports without retiring source", wantSource: "active", wantTarget: "pending", wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			recordingID := dbtest.Recording(t, pool)
			if tc.originalPresent {
				seedOriginalAsset(t, pool, roots["test-store"], recordingID, "sites/default/transfer/original.ts", []byte("original source"))
			}
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
			worker := StorageTransferWorker{Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots}
			if err := worker.Work(context.Background(), jobs.StorageTransferArgs{
				MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint,
			}); err != nil {
				t.Fatalf("Work with corrupt sole derived copy: %v", err)
			}

			var sourceState, targetState string
			if err := pool.QueryRow(context.Background(),
				"SELECT source.state, target.state FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id AND target.generation = source.generation WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
				assetID, "test-store", "archive").Scan(&sourceState, &targetState); err != nil {
				t.Fatalf("reading copy states after derived source mismatch: %v", err)
			}
			if sourceState != tc.wantSource || targetState != tc.wantTarget {
				t.Fatalf("source/target copy states = %q/%q, want %q/%q", sourceState, targetState, tc.wantSource, tc.wantTarget)
			}
			var attempts int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts attempt JOIN media_asset_copies copy ON copy.id = attempt.copy_id WHERE copy.media_asset_id = $1`, assetID).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if attempts != tc.wantAttempts {
				t.Fatalf("transfer attempt rows = %d, want %d", attempts, tc.wantAttempts)
			}
			if fileExists(filepath.Join(roots["archive"], filepath.FromSlash(relPath))) {
				t.Fatal("mismatched derived source was published to the destination")
			}
		})
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
	media := pgstore.NewMedia(pool, "test-store", "")
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
	args := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}
	if err := worker.Work(context.Background(), args); err != nil {
		t.Fatalf("Work with damaged uncheckpointed temp tail: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Fatalf("derived asset state after damaged temp = %q, want active", got)
	}
	var sourceState, targetState string
	var attemptCount int32
	if err := pool.QueryRow(context.Background(),
		"SELECT source.state, target.state, attempts.attempt_count FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id AND target.generation = source.generation JOIN media_asset_transfer_attempts attempts ON attempts.copy_id = target.id WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
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
		"SELECT source.state, target.state FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id AND target.generation = source.generation WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
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
	media := pgstore.NewMedia(pool, "test-store", "")
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
	oldJob := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: oldFingerprint}
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
		MediaAssetID: assetID, Store: "cold", PolicyFingerprint: newFingerprint,
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
	deleteWorker := DeleteReconcileWorker{
		MediaDelete: media, Storage: media, StorageTransfer: media, StorageTransferPolicy: newConfig,
		RecordingPurge: pgstore.NewRecordings(pool, "test-store"), Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["test-store"], MediaStoreName: "test-store", TransferOverlap: time.Hour,
	}
	if err := deleteWorker.Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("deleting stale source copy: %v", err)
	}
	if fileExists(filepath.Join(roots["test-store"], "sites/default/transfer/source.ts")) {
		t.Fatal("stale source file remains after delete reconcile")
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
	reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg}
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
	if got.MediaAssetID != assetID || got.Store != "archive" || got.PolicyFingerprint != fingerprint {
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

func TestStorageTransferReconcileReportsExhaustionAndClearsObsoleteAttempts(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive", "cold")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("attempted transfer"))
	media := pgstore.NewMedia(pool, "test-store", "")
	archiveConfig := transferTestConfig("archive", "archive", "test-store")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, archiveConfig), archiveConfig.FSStoreNames(), archiveConfig.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing failed transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	for i := int32(0); i < storageTransferAttemptLimit; i++ {
		if err := media.RecordMediaAssetTransferFailure(context.Background(), transfer.CopyID, "destination unavailable"); err != nil {
			t.Fatal(err)
		}
	}
	metrics.StorageTransferWaitingAssets.Set(0)
	metrics.StorageTransferWaitingBytes.Set(0)
	metrics.StorageTransferExhaustedAssets.Set(0)
	t.Cleanup(func() {
		metrics.StorageTransferWaitingAssets.Set(0)
		metrics.StorageTransferWaitingBytes.Set(0)
		metrics.StorageTransferExhaustedAssets.Set(0)
	})
	firstEnqueuer := newRecordingEnqueuer()
	firstPass := StorageTransferReconcileWorker{Enqueuer: firstEnqueuer, Storage: media, Config: archiveConfig}
	if err := firstPass.Work(context.Background(), jobs.StorageTransferReconcileArgs{}); err != nil {
		t.Fatal(err)
	}
	if got := len(firstEnqueuer.Enqueued("storage_transfer")); got != 0 {
		t.Fatalf("jobs after exhausted attempts = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferWaitingAssets); got != 0 {
		t.Fatalf("waiting assets after exhausted attempts = %v, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferExhaustedAssets); got != 1 {
		t.Fatalf("exhausted assets = %v, want 1", got)
	}

	coldConfig := transferTestConfig("cold", "archive", "cold", "test-store")
	secondEnqueuer := newRecordingEnqueuer()
	secondPass := StorageTransferReconcileWorker{Enqueuer: secondEnqueuer, Storage: media, Config: coldConfig}
	if err := secondPass.Work(context.Background(), jobs.StorageTransferReconcileArgs{}); err != nil {
		t.Fatal(err)
	}
	if got := len(secondEnqueuer.Enqueued("storage_transfer")); got != 1 {
		t.Fatalf("jobs after desired store changes = %d, want 1", got)
	}
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("obsolete attempts after policy switch = %d, want 0", attempts)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferWaitingAssets); got != 1 {
		t.Fatalf("waiting assets after policy switch = %v, want 1", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferExhaustedAssets); got != 0 {
		t.Fatalf("exhausted assets after policy switch = %v, want 0", got)
	}
}

func TestStorageTransferPendingReclaimSelectionRechecksObsoleteState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, pool *pgxpool.Pool, recordingID, assetID int64)
		policy  func() config.StorageConfig
		wantOne bool
	}{
		{name: "still desired", policy: func() config.StorageConfig { return transferTestConfig("archive", "archive", "test-store") }},
		{
			name: "trashed recording",
			change: func(t *testing.T, pool *pgxpool.Pool, recordingID, _ int64) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID); err != nil {
					t.Fatal(err)
				}
			},
			policy:  func() config.StorageConfig { return transferTestConfig("archive", "archive", "test-store") },
			wantOne: true,
		},
		{
			name: "new generation",
			change: func(t *testing.T, pool *pgxpool.Pool, _, assetID int64) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET derived_generation = 1 WHERE id = $1`, assetID); err != nil {
					t.Fatal(err)
				}
			},
			policy:  func() config.StorageConfig { return transferTestConfig("archive", "archive", "test-store") },
			wantOne: true,
		},
		{
			name: "different desired placement",
			policy: func() config.StorageConfig {
				return transferTestConfig("cold", "archive", "cold", "test-store")
			},
			wantOne: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive", "cold")
			recordingID := dbtest.Recording(t, pool)
			profile := "web"
			assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/pending.mp4", []byte("encoded"))
			initialPolicy := transferTestConfig("archive", "archive", "test-store", "cold")
			media := pgstore.NewMedia(pool, "archive", "")
			transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, initialPolicy), initialPolicy.FSStoreNames(), initialPolicy.Derived, storageTransferAttemptLimit)
			if err != nil || !ok {
				t.Fatalf("preparing pending copy = (%+v, %t, %v)", transfer, ok, err)
			}
			if tc.change != nil {
				tc.change(t, pool, recordingID, assetID)
			}
			policy := tc.policy()
			copies, err := media.ListObsoletePendingMediaAssetTransferCopies(context.Background(), mustOriginalRules(t, policy), policy.FSStoreNames(), policy.Derived, 10)
			if err != nil {
				t.Fatalf("listing obsolete pending copies: %v", err)
			}
			if gotOne := len(copies) == 1; gotOne != tc.wantOne {
				t.Fatalf("obsolete pending copy count = %d, want one=%t (rows: %+v)", len(copies), tc.wantOne, copies)
			}
		})
	}
}

func TestStorageTransferPendingReclaimRechecksAfterRecordingRestore(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/restored.mp4", []byte("encoded"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMedia(pool, "archive", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing pending copy = (%+v, %t, %v)", transfer, ok, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	candidates, err := media.ListObsoletePendingMediaAssetTransferCopies(context.Background(), mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("obsolete candidates before restore = %+v, %v; want one", candidates, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = NULL WHERE id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	called := false
	reclaimed, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(
		context.Background(), candidates[0], mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived,
		func() error { called = true; return nil },
	)
	if err != nil {
		t.Fatalf("reclaiming a restored pending copy: %v", err)
	}
	if reclaimed || called {
		t.Fatalf("reclaim after restore = (reclaimed %t, delete called %t), want no-op", reclaimed, called)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("pending copy after restore = %q, want pending", state)
	}
}

func prepareUntilEncodedPendingOriginal(t *testing.T, pool *pgxpool.Pool, roots map[string]string, recordingID int64) (int64, store.MediaAssetTransfer, config.StorageConfig, int64) {
	t.Helper()
	originalRelPath := "sites/default/retention/pending-original.ts"
	originalID := seedOriginalAsset(t, pool, roots["test-store"], recordingID, originalRelPath, []byte("source-original"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMedia(pool, "archive", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(
		context.Background(), originalID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, storageTransferAttemptLimit,
	)
	if err != nil || !ok {
		t.Fatalf("preparing pending original transfer = (%+v, %t, %v)", transfer, ok, err)
	}

	profile := "web"
	addDerived := func(kind, relPath string, profile *string, content []byte) int64 {
		t.Helper()
		assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, kind, profile, relPath, content)
		archivePath := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, content, 0o644); err != nil {
			t.Fatal(err)
		}
		transferTestAddCopy(t, pool, assetID, "archive", relPath)
		return assetID
	}
	addDerived(store.AssetKindEncoded, "sites/default/retention/encoded.mp4", &profile, []byte("encoded"))
	addDerived(store.AssetKindThumbnail, "sites/default/retention/thumbnail.jpg", nil, []byte("thumbnail"))
	seekTilesID := addDerived(store.AssetKindSeekTiles, "sites/default/retention/tiles.jpg", nil, []byte("tiles"))
	markRecordingUntilEncoded(t, pool, recordingID, []string{profile})
	var eligible bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM until_encoded_deletable_originals('archive') WHERE asset_id = $1)`, originalID).Scan(&eligible); err != nil {
		t.Fatalf("checking desired-store until_encoded eligibility: %v", err)
	}
	if !eligible {
		t.Fatal("until_encoded fixture does not qualify for original deletion")
	}
	return originalID, transfer, cfg, transferTestCopyID(t, pool, seekTilesID, "archive")
}

func TestDeleteReconcileReclaimsPendingOriginalAfterUntilEncoded(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	originalID, transfer, cfg, _ := prepareUntilEncodedPendingOriginal(t, pool, roots, recordingID)
	targetPath := filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("source-original"), 0o644); err != nil {
		t.Fatal(err)
	}

	newWorker := func(storeName string) *DeleteReconcileWorker {
		media := pgstore.NewMedia(pool, storeName, cfg.Derived)
		return &DeleteReconcileWorker{
			MediaDelete: media, Storage: media, StorageTransfer: media, StorageTransferPolicy: cfg,
			RecordingPurge: pgstore.NewRecordings(pool, storeName), Breakers: pgstore.NewBreakers(pool),
			MediaDir: roots[storeName], MediaStoreName: storeName, TrashRetention: time.Hour,
		}
	}
	if err := newWorker("test-store").Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("deleting source original after until_encoded: %v", err)
	}
	if fileExists(filepath.Join(roots["test-store"], filepath.FromSlash(transfer.RelPath))) {
		t.Fatal("until_encoded source original remains after deletion reconcile")
	}
	var sourceState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'test-store'`, originalID).Scan(&sourceState); err != nil {
		t.Fatal(err)
	}
	if sourceState != "deleted" {
		t.Fatalf("until_encoded source copy state = %q, want deleted", sourceState)
	}
	if !fileExists(targetPath) {
		t.Fatal("fixture destination should remain before its pending reservation is reclaimed")
	}
	if err := newWorker("archive").Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("reclaiming until_encoded pending destination: %v", err)
	}
	if fileExists(targetPath) {
		t.Fatal("pending destination remains after until_encoded made its transfer obsolete")
	}
	var targetState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&targetState); err != nil {
		t.Fatal(err)
	}
	if targetState != "deleted" {
		t.Fatalf("until_encoded pending copy state = %q, want deleted", targetState)
	}
}

func TestStorageTransferPendingReclaimRechecksUntilEncodedEligibility(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	_, transfer, cfg, seekTilesCopyID := prepareUntilEncodedPendingOriginal(t, pool, roots, recordingID)
	media := pgstore.NewMedia(pool, "archive", "")
	candidates, err := media.ListObsoletePendingMediaAssetTransferCopies(
		context.Background(), mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived, 10,
	)
	if err != nil || len(candidates) != 1 || candidates[0].CopyID != transfer.CopyID {
		t.Fatalf("until_encoded obsolete candidates = %+v, %v; want pending original copy %d", candidates, err, transfer.CopyID)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, seekTilesCopyID); err != nil {
		t.Fatalf("making until_encoded eligibility false: %v", err)
	}
	var eligible bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM until_encoded_deletable_originals('archive') WHERE asset_id = $1)`, transfer.MediaAssetID).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("fixture still qualifies for until_encoded after desired seek tiles were marked missing")
	}
	called := false
	reclaimed, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(
		context.Background(), candidates[0], mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.Derived,
		func() error { called = true; return nil },
	)
	if err != nil {
		t.Fatalf("rechecking pending original retention: %v", err)
	}
	if reclaimed || called {
		t.Fatalf("reclaim after until_encoded eligibility changed = (reclaimed %t, delete called %t), want no-op", reclaimed, called)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("pending copy after retention recheck = %q, want pending", state)
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
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM until_encoded_deletable_originals('archive') WHERE asset_id = $1`, originalID).Scan(&count); err != nil {
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
	seekTilesCopyID := transferTestCopyID(t, pool, seekTilesID, "archive")
	if _, err := pool.Exec(context.Background(), `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, seekTilesCopyID); err != nil {
		t.Fatalf("marking desired seek tiles copy missing: %v", err)
	}
	assertOriginalEligibility(false)
	if _, err := pool.Exec(context.Background(), `DELETE FROM missing_media_assets WHERE media_asset_copy_id = $1`, seekTilesCopyID); err != nil {
		t.Fatal(err)
	}
	assertOriginalEligibility(true)

	cfg := transferTestConfig("archive", "archive", "test-store")
	candidates, err := pgstore.NewMedia(pool, "test-store", "").ListMediaAssetTransferCandidates(
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
	media := pgstore.NewMedia(pool, "test-store", "")
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
	archiveCopyID := transferTestCopyID(t, pool, assetID, "archive")
	if _, err := pool.Exec(context.Background(), `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, archiveCopyID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "archive", 0, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates with desired copy marked missing = %+v, %v; want none", candidates, err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM missing_media_assets WHERE media_asset_copy_id = $1`, archiveCopyID); err != nil {
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

func TestStorageTransferDeletionKeepsCopiesWhenPolicyHasNoDesiredStore(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/no-desired/source.ts"
	})
	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/no-desired/source.ts")
	transferTestAddCopy(t, pool, assetID, "cold", "sites/default/no-desired/source.ts")
	media := pgstore.NewMedia(pool, "archive", "")
	rules := []byte(`[]`)

	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), rules, "", 0, 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates with no desired store = %+v, %v; want none", candidates, err)
	}
	candidate := store.MediaAssetCopyDeleteCandidate{
		CopyID:       transferTestCopyID(t, pool, assetID, "archive"),
		MediaAssetID: assetID, Store: "archive", RelPath: "sites/default/no-desired/source.ts",
	}
	if _, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), candidate, rules, "", 0); err != nil || marked {
		t.Fatalf("marking copy with no desired store = %v, %v; want false, nil", marked, err)
	}
	var activeCopies int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM media_asset_copies
WHERE media_asset_id = $1 AND state = 'active' AND store IN ('archive', 'cold')`, assetID).Scan(&activeCopies); err != nil {
		t.Fatalf("counting surviving copies: %v", err)
	}
	if activeCopies != 2 {
		t.Fatalf("active copies after no-policy deletion = %d, want both copies retained", activeCopies)
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

	media := pgstore.NewMedia(pool, "test-store", "")
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
