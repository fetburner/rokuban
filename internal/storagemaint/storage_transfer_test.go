package storagemaint

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mediapublish"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func transferTestConfig(desiredStore string, stores ...string) config.StorageConfig {
	storeConfigs := make(map[string]config.StoreConfig, len(stores))
	for _, name := range stores {
		storeConfigs[name] = config.StoreConfig{Kind: "fs"}
	}
	cfg := config.StorageConfig{
		Stores:    storeConfigs,
		Originals: []config.OriginalStoreRule{{Store: desiredStore}},
		Derived:   desiredStore,
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
	return cfg
}

func transferTestRoots(t *testing.T, stores ...string) map[string]string {
	t.Helper()
	roots := make(map[string]string, len(stores))
	for _, name := range stores {
		root := t.TempDir()
		testutil.RequireNoError(t, mediapath.WriteStoreMarker(root, name), fmt.Sprintf("writing %s store marker", name))
		roots[name] = root
	}
	return roots
}

func transferTestObjectStores(t *testing.T, roots map[string]string) map[string]objstore.Store {
	t.Helper()
	objects := make(map[string]objstore.Store, len(roots))
	for name, root := range roots {
		store, err := fsstore.New(root)
		testutil.RequireNoError(t, err, fmt.Sprintf("creating %s test object store", name))
		objects[name] = store
	}
	return objects
}

func transferTestOriginal(t *testing.T, pool *pgxpool.Pool, sourceRoot string, content []byte) int64 {
	t.Helper()
	recordingID := dbtest.Recording(t, pool)
	return seedOriginalAsset(t, pool, sourceRoot, recordingID, "sites/default/transfer/source.ts", content)
}

func transferTestCopyID(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName string) int64 {
	t.Helper()
	var id int64
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, storeName).Scan(&id), fmt.Sprintf("reading copy ID for asset %d in %s", assetID, storeName))
	return id
}

func transferTestCopyRelPath(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName string) string {
	t.Helper()
	var relPath string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT rel_path FROM media_asset_copies
WHERE media_asset_id = $1 AND store = $2 AND state <> 'deleted'
ORDER BY id DESC LIMIT 1`, assetID, storeName).Scan(&relPath), fmt.Sprintf("reading copy rel_path for asset %d in %s", assetID, storeName))
	return relPath
}

func transferTestLatestCopyRelPath(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName string) string {
	t.Helper()
	var relPath string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT COALESCE((
    SELECT rel_path FROM media_asset_copies
    WHERE media_asset_id = $1 AND store = $2
    ORDER BY id DESC LIMIT 1
), '')`, assetID, storeName).Scan(&relPath), fmt.Sprintf("reading latest copy rel_path for asset %d in %s", assetID, storeName))
	return relPath
}

func transferTestPayloadFiles(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() != mediapath.StoreMarkerFileName &&
			!mediapath.IsGeneratedAssetTempFile(entry.Name()) && !mediapath.IsStorageTransferTempFile(entry.Name()) {
			paths = append(paths, path)
		}
		return nil
	})
	testutil.RequireNoError(t, err, fmt.Sprintf("walking transfer test store %s", root))
	return paths
}

func transferTestAddCopy(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName, relPath string) {
	t.Helper()
	testutil.MustExec(t, pool, fmt.Sprintf("adding active copy in %s", storeName), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT a.id, $2, $3, a.derived_generation, 'active'
FROM media_assets a WHERE a.id = $1
ON CONFLICT (media_asset_id, store, generation) WHERE state <> 'deleted' DO UPDATE
SET rel_path = EXCLUDED.rel_path, state = 'active', deleted_at = NULL, created_at = now()`, assetID, storeName, relPath)
}

// prepareAdoptedTransferTemp は、宛先を採用する時点の転送 temp を作る。
// 前の試行が世代 1 の temp を書いて lease を切った状態にする（次の試行は世代 2 を取り、前の世代の temp を見る）。
// owner が "linked" なら宛先を temp への link にし、"foreign" なら別の inode のままにする。空なら何もしない。
func prepareAdoptedTransferTemp(t *testing.T, media *pgstore.Media, cfg config.StorageConfig, assetID int64, targetPath string, content []byte, owner string) string {
	t.Helper()
	if owner == "" {
		return ""
	}
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10)
	if err != nil || !ok {
		t.Fatalf("preparing transfer whose destination is adopted = (%+v, %t, %v)", transfer, ok, err)
	}
	leaseOwner := uuid.New()
	progress, acquired, err := media.AcquireMediaAssetTransferProgress(context.Background(), transfer.CopyID, leaseOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquiring progress for the previous attempt = (%+v, %t, %v)", progress, acquired, err)
	}
	tempPath := transferTempPath(targetPath, assetID, transfer.CopyID, progress.WriterGeneration)
	testutil.RequireNoError(t, os.WriteFile(tempPath, content, 0o644), "os.WriteFile")
	if owner == "linked" {
		testutil.RequireNoError(t, os.Remove(targetPath), "os.Remove")
		testutil.RequireNoError(t, os.Link(tempPath, targetPath), "linking destination to transfer temp")
	}
	testutil.RequireNoError(t, media.ReleaseMediaAssetTransferProgress(context.Background(), transfer.CopyID, leaseOwner), "releasing progress of the previous attempt")
	return tempPath
}

// assertAdoptedTransferTemp は、宛先の採用後に前の世代の temp が残る・残らないを確かめる。
func assertAdoptedTransferTemp(t *testing.T, tempPath, owner string) {
	t.Helper()
	if tempPath == "" {
		return
	}
	switch owner {
	case "linked":
		if fileExists(tempPath) {
			t.Errorf("%q remains after adopting the destination this transfer published", tempPath)
		}
	case "foreign":
		if !fileExists(tempPath) {
			t.Errorf("%q was removed although the adopted destination is not this transfer's temp", tempPath)
		}
	}
}

// saveTransferProgressForTest は、slice が partial まで書いて保存した状態を作る。
// temp は今の世代の名前で partial を書き、fsink.SaveCheckpoint と同じ経路で保存して lease を切る。
// 次の Work はきれいな引き継ぎとして、同じ世代の続きから書く。
func saveTransferProgressForTest(t *testing.T, media *pgstore.Media, transfer store.MediaAssetTransfer, root string, partial []byte) string {
	t.Helper()
	ctx := context.Background()
	owner := uuid.New()
	progress, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, transfer.CopyID, owner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquiring progress for saved slice = (%+v, %t, %v)", progress, acquired, err)
	}
	targetPath := filepath.Join(root, filepath.FromSlash(transfer.RelPath))
	tempPath := transferTempPath(targetPath, transfer.MediaAssetID, transfer.CopyID, progress.WriterGeneration)
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(tempPath), 0o755), "os.MkdirAll")
	file, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	testutil.RequireNoError(t, err, "os.OpenFile")
	defer func() { _ = file.Close() }()
	_, err = file.Write(partial)
	testutil.RequireNoError(t, err, "file.Write")
	hasher := sha256.New()
	_, err = hasher.Write(partial)
	testutil.RequireNoError(t, err, "hasher.Write")
	testutil.RequireNoError(t, fsink.SaveCheckpoint(file, int64(len(partial)), hasher, func(c fsink.Checkpoint) error {
		return media.SaveMediaAssetTransferProgress(ctx, transfer.CopyID, owner, progress.WriterGeneration, c.Offset, c.HashState, nil)
	}), "saving transfer progress")
	return tempPath
}

func TestPrepareMediaAssetTransferKeepsPendingRelPathAcrossRetries(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("pending transfer path"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	prepare := func() store.MediaAssetTransfer {
		t.Helper()
		transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
		if err != nil || !ok {
			t.Fatalf("PrepareMediaAssetTransfer = (%+v, %t, %v)", transfer, ok, err)
		}
		return transfer
	}
	first := prepare()
	second := prepare()
	if second.CopyID != first.CopyID {
		t.Fatalf("pending copy ID after retry = %d, want %d", second.CopyID, first.CopyID)
	}
	if second.RelPath != first.RelPath {
		t.Fatalf("pending rel_path after retry = %q, want the reserved path %q", second.RelPath, first.RelPath)
	}
}

func TestStorageTransferWorkerCopiesAndAdopts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		existing bool
		// resume は、前の slice が半分まで書いて保存した状態から続けることを表す（きれいな引き継ぎ）。
		resume bool
		// tempOwner は、宛先を採用する時点の転送 temp の関係。"linked" は link 済み・commit 前に落ちた転送
		// （temp と宛先が同じ inode）、"foreign" は別の inode（他人の宛先）を表す。
		tempOwner string
	}{
		{name: "copy computes and stores SHA-256"},
		{name: "matching restored destination is adopted", existing: true},
		{name: "adopting a destination linked from this transfer's temp removes the temp", existing: true, tempOwner: "linked"},
		{name: "adopting a destination that is not this transfer's temp keeps the temp", existing: true, tempOwner: "foreign"},
		{name: "saved progress resumes the same generation", resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			content := []byte("transfer-asset-bytes")
			assetID := transferTestOriginal(t, pool, roots["test-store"], content)
			cfg := transferTestConfig("archive", "archive", "test-store")
			fingerprint, err := cfg.PolicyFingerprint()
			testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
			media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
			transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10)
			if err != nil || !ok {
				t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
			}
			targetPath := filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath))
			if tc.existing {
				testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755), "os.MkdirAll")
				testutil.RequireNoError(t, os.WriteFile(targetPath, content, 0o644), "os.WriteFile")
			}
			if tc.resume {
				saveTransferProgressForTest(t, media, transfer, roots["archive"], content[:len(content)/2])
			}
			adoptedTempPath := prepareAdoptedTransferTemp(t, media, cfg, assetID, targetPath, content, tc.tempOwner)
			worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
			testutil.RequireNoError(t, settleTransferWork(t, func() error {
				return worker.Work(context.Background(), jobs.StorageTransferArgs{
					MediaAssetID: assetID,
					Store:        "archive", PolicyFingerprint: fingerprint,
				})
			}), "StorageTransferWorker.Work")
			assertAdoptedTransferTemp(t, adoptedTempPath, tc.tempOwner)
			got, err := os.ReadFile(targetPath)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("destination bytes = %q, %v; want %q", got, err, content)
			}
			expectedSHA := sha256.Sum256(content)
			var assetSHA []byte
			var copyState string
			testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&assetSHA), "reading observed SHA-256")
			if !bytes.Equal(assetSHA, expectedSHA[:]) {
				t.Fatalf("media_assets.sha256 = %x, want %x", assetSHA, expectedSHA)
			}
			copyState = transferTestCopyState(t, pool, assetID)
			if copyState != "active" {
				t.Fatalf("destination copy state = %q, want active", copyState)
			}
			var progressRows int
			testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copy_transfer_progress`).Scan(&progressRows), "counting transfer progress rows")
			if progressRows != 0 {
				t.Fatalf("transfer progress rows after the copy became active = %d, want 0", progressRows)
			}
		})
	}
}

// encoded の字幕 key の転送は、内容の違う既存の key を所有記録の有無にかかわらず衝突として扱い、消さない。
// source に字幕が無いときは、この転送が作った（所有記録のある）宛先の字幕だけを消す。
func TestStorageTransferWorkerEncodedSubtitleKeys(t *testing.T) {
	t.Parallel()
	const sourceCaptions = "WEBVTT\n\nsource captions\n"
	const otherCaptions = "WEBVTT\n\nother captions\n"
	for _, tc := range []struct {
		name           string
		existingVideo  bool
		sourceSubtitle bool
		targetSubtitle string
		targetOwned    bool
		wantActive     bool
		wantTargetVTT  string // 空ならファイルが無いこと
	}{
		{name: "new destination copies sidecar", sourceSubtitle: true, wantActive: true, wantTargetVTT: sourceCaptions},
		{name: "pre-existing different sidecar is a collision and is kept", existingVideo: true, sourceSubtitle: true, targetSubtitle: otherCaptions, wantTargetVTT: otherCaptions},
		{name: "owned different sidecar is a collision and is kept", sourceSubtitle: true, targetSubtitle: otherCaptions, targetOwned: true, wantTargetVTT: otherCaptions},
		{name: "unowned sidecar stays when the source has none", existingVideo: true, targetSubtitle: otherCaptions, wantActive: true, wantTargetVTT: otherCaptions},
		{name: "owned sidecar is removed when the source has none", targetSubtitle: otherCaptions, targetOwned: true, wantActive: true},
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
			testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
			if tc.sourceSubtitle {
				testutil.RequireNoError(t, os.WriteFile(filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath)), []byte(sourceCaptions), 0o644), "os.WriteFile")
			}
			cfg := transferTestConfig("archive", "archive", "test-store")
			fingerprint, err := cfg.PolicyFingerprint()
			testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
			media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
			transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
			if err != nil || !ok {
				t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
			}
			targetRelPath := transfer.RelPath
			targetSubtitleRelPath, err := mediapath.SubtitleSibling(targetRelPath)
			testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
			targetVideo := filepath.Join(roots["archive"], filepath.FromSlash(targetRelPath))
			targetSubtitle := filepath.Join(roots["archive"], filepath.FromSlash(targetSubtitleRelPath))
			testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(targetVideo), 0o755), "os.MkdirAll")
			if tc.existingVideo {
				testutil.RequireNoError(t, os.WriteFile(targetVideo, video, 0o644), "os.WriteFile")
			}
			if tc.targetSubtitle != "" {
				testutil.RequireNoError(t, os.WriteFile(targetSubtitle, []byte(tc.targetSubtitle), 0o644), "os.WriteFile")
			}
			if tc.targetOwned {
				if ownership, err := media.MarkMediaAssetTransferObjectOwned(context.Background(), transfer.CopyID, "archive", targetSubtitleRelPath); err != nil || ownership != store.TransferObjectOwned {
					t.Fatalf("recording subtitle ownership = (%d, %v)", ownership, err)
				}
			}
			worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
			testutil.RequireNoError(t, settleTransferWork(t, func() error {
				return worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint})
			}), "StorageTransferWorker.Work")

			got, err := os.ReadFile(targetSubtitle)
			if tc.wantTargetVTT != "" {
				if err != nil || string(got) != tc.wantTargetVTT {
					t.Fatalf("destination subtitle = %q, %v; want %q", got, err, tc.wantTargetVTT)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination subtitle read error = %v, want removed", err)
			}
			state := transferTestCopyState(t, pool, assetID)
			wantState := "pending"
			if tc.wantActive {
				wantState = "active"
			}
			if state != wantState {
				t.Fatalf("destination copy state = %q, want %q", state, wantState)
			}
			var attempts int
			testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts a JOIN media_asset_copies c ON c.id = a.copy_id WHERE c.media_asset_id = $1`, assetID).Scan(&attempts), "counting transfer attempts for the asset")
			if (attempts == 1) == tc.wantActive {
				t.Errorf("transfer failure attempts = %d, want a recorded failure exactly when the copy is not activated (active=%t)", attempts, tc.wantActive)
			}
		})
	}
}

func TestDeleteReconcilePlacementDeletionRemovesEncodedSubtitleSidecar(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/captions.mp4"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, []byte("encoded-video"))
	subtitleRelPath, err := mediapath.SubtitleSibling(relPath)
	testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\nsource captions\n"), 0o644), "os.WriteFile")
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(filepath.Join(roots["archive"], filepath.FromSlash(relPath))), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(roots["archive"], filepath.FromSlash(relPath)), []byte("encoded-video"), 0o644), "os.WriteFile")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\nsource captions\n"), 0o644), "os.WriteFile")
	transferTestAddCopy(t, pool, assetID, "archive", relPath)
	testutil.MustExec(t, pool, "aging the archive copy past the overlap", `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'archive'`, assetID)
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	objects := transferTestObjectStores(t, roots)
	worker := DeleteReconcileWorker{
		DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: media, StorageTransfer: media, RecordingPurge: pgstore.NewRecordings(pool, "test-store")}, Storage: media,
		Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["test-store"], StorageObjects: testStorageObjects(t, "test-store", roots["test-store"]), MediaStoreName: "test-store", TransferOverlap: time.Hour,
		DerivedObjects: objects["archive"],
	}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
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
	testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(roots["test-store"], filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\nabandoned captions\n"), 0o644), "os.WriteFile")
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, cfg))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing abandoned copy = (%+v, %t, %v)", transfer, ok, err)
	}
	testutil.RequireNoError(t, media.RecordMediaAssetTransferFailure(context.Background(), transfer.CopyID, "destination interrupted"), "recording failed transfer attempt")
	targetRelPath := transfer.RelPath
	targetSubtitleRelPath, err := mediapath.SubtitleSibling(targetRelPath)
	testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
	objects := transferTestObjectStores(t, roots)
	for _, object := range []struct {
		key         string
		content     []byte
		contentType string
	}{{targetRelPath, video, "video/mp4"}, {targetSubtitleRelPath, []byte("abandoned captions"), "text/vtt; charset=utf-8"}} {
		sum := sha256.Sum256(object.content)
		testutil.RequireNoError(t, objects["archive"].Put(context.Background(), object.key, bytes.NewReader(object.content), int64(len(object.content)), sum, object.contentType), fmt.Sprintf("putting owned pending transfer object %q", object.key))
		ownership, err := media.MarkMediaAssetTransferObjectOwned(context.Background(), transfer.CopyID, "archive", object.key)
		if err != nil || ownership != store.TransferObjectOwned {
			t.Fatalf("recording ownership of pending transfer object %q = (%d, %v)", object.key, ownership, err)
		}
	}
	testutil.MustExec(t, pool, "retiring the test-store copy", `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = 'test-store'`, assetID)
	testutil.MustExec(t, pool, "aging the recording past the purge window", `UPDATE recordings SET deleted_at = now() - interval '2 hours' WHERE id = $1`, recordingID)
	derivedDeleteStores, storeErr := store.NewDerivedObjectDeleteStores(media, media, media)
	testutil.RequireNoError(t, storeErr, "creating derived object delete stores")
	worker := DeleteReconcileWorker{
		DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: media, StorageTransfer: media, RecordingPurge: pgstore.NewRecordings(pool, "archive"), MediaPendingReclaim: media, DerivedObjectDeleteStores: derivedDeleteStores}, Storage: media,
		Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["archive"], StorageObjects: testStorageObjects(t, "archive", roots["archive"]), MediaStoreName: "archive", TrashRetention: time.Hour,
		OrphanAge: time.Hour, OrphanMTimeGrace: time.Minute,
		DerivedObjects: objects["archive"], DerivedObjectStoreName: "archive",
	}
	deletedBefore := promtestutil.ToFloat64(metrics.DeleteReconcileDeleted.WithLabelValues("pending_transfer_reclaim"))
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := promtestutil.ToFloat64(metrics.DeleteReconcileDeleted.WithLabelValues("pending_transfer_reclaim")); got != deletedBefore+1 {
		t.Errorf("pending transfer reclaim metric = %v, want one increment from %v", got, deletedBefore)
	}
	for _, path := range []string{
		filepath.Join(roots["archive"], filepath.FromSlash(targetRelPath)),
		filepath.Join(roots["archive"], filepath.FromSlash(targetSubtitleRelPath)),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("reclaimed pending path %q stat error = %v, want not-exist", path, err)
		}
	}
	var copyState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&copyState), "reading copy state after reclaim")
	if copyState != "deleted" {
		t.Errorf("abandoned pending copy state = %q, want deleted", copyState)
	}
	var attempts int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts WHERE copy_id = $1`, transfer.CopyID).Scan(&attempts), "counting transfer attempts after reclaim")
	if attempts != 0 {
		t.Errorf("retired pending copy attempts = %d, want 0", attempts)
	}
	var purged bool
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT purged_at IS NOT NULL FROM recordings WHERE id = $1`, recordingID).Scan(&purged), "reading recording purged flag")
	if !purged {
		t.Fatal("recording remains unpurged after its last pending FS copy was reclaimed")
	}
}

func TestDeleteReconcilePreservesPreexistingDestinationAfterPendingTransferReclaim(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/restored.mp4"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, []byte("valid encoded video"))
	collision := []byte("pre-existing restored file")
	collisionSubtitle := []byte("pre-existing captions")
	transferConfig := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, transferConfig))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", transferConfig.FSStoreNames(), transferConfig.FSStoreNames(), transferConfig.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing pending copy = (%+v, %t, %v)", transfer, ok, err)
	}
	targetRelPath := transfer.RelPath
	targetSubtitleRelPath, err := mediapath.SubtitleSibling(targetRelPath)
	testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
	targetPath := filepath.Join(roots["archive"], filepath.FromSlash(targetRelPath))
	targetSubtitlePath := filepath.Join(roots["archive"], filepath.FromSlash(targetSubtitleRelPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(targetPath, collision, 0o644), "os.WriteFile")
	testutil.RequireNoError(t, os.WriteFile(targetSubtitlePath, collisionSubtitle, 0o644), "os.WriteFile")

	fingerprint, err := transferConfig.PolicyFingerprint()
	testutil.RequireNoError(t, err, "transferConfig.PolicyFingerprint")
	objects := transferTestObjectStores(t, roots)
	transferWorker := StorageTransferWorker{Storage: media, Config: transferConfig, Roots: roots, ObjectStores: objects, ScratchDir: t.TempDir()}
	testutil.RequireNoError(t, settleTransferWork(t, func() error {
		return transferWorker.Work(context.Background(), jobs.StorageTransferArgs{
			MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint,
		})
	}), "StorageTransferWorker.Work on destination collision")
	if got, err := os.ReadFile(targetPath); err != nil || !bytes.Equal(got, collision) {
		t.Fatalf("destination after rejected transfer = %q, %v; want preserved collision %q", got, err, collision)
	}
	if got, err := os.ReadFile(targetSubtitlePath); err != nil || !bytes.Equal(got, collisionSubtitle) {
		t.Fatalf("subtitle after rejected transfer = %q, %v; want preserved collision %q", got, err, collisionSubtitle)
	}

	deleteConfig := transferTestConfig("test-store", "archive", "test-store")
	deleteMedia := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, deleteConfig))
	deleteWorker := DeleteReconcileWorker{
		DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: deleteMedia, StorageTransfer: deleteMedia, RecordingPurge: pgstore.NewRecordings(pool, "archive"), MediaPendingReclaim: deleteMedia}, Storage: deleteMedia,
		Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["archive"], StorageObjects: testStorageObjects(t, "archive", roots["archive"]), MediaStoreName: "archive",
		DerivedObjects: objects["archive"], DerivedObjectStoreName: "archive",
	}
	testutil.RequireNoError(t, deleteWorker.Work(context.Background(), jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got, err := os.ReadFile(targetPath); err != nil || !bytes.Equal(got, collision) {
		t.Errorf("destination after pending reclaim = %q, %v; want preserved collision %q", got, err, collision)
	}
	if got, err := os.ReadFile(targetSubtitlePath); err != nil || !bytes.Equal(got, collisionSubtitle) {
		t.Errorf("subtitle after pending reclaim = %q, %v; want preserved collision %q", got, err, collisionSubtitle)
	}
	copyState := transferTestCopyState(t, pool, assetID)
	if copyState != "deleted" {
		t.Errorf("obsolete pending copy state = %q, want deleted", copyState)
	}
	var ownedObjects int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_objects WHERE copy_id = (SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive')`, assetID).Scan(&ownedObjects), "counting transfer objects owned by the archive copy")
	if ownedObjects != 0 {
		t.Errorf("pending transfer ownership rows after reclaim = %d, want none", ownedObjects)
	}
}

func TestDeleteReconcileRetriesPendingReclaimWhenSubtitleRemovalFails(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/retry-caption-cleanup.mp4"
	video := []byte("owned encoded video")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, video)
	transferConfig := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, transferConfig))
	transfer, ok, err := media.PrepareMediaAssetTransfer(
		context.Background(), assetID, "archive", transferConfig.FSStoreNames(), transferConfig.FSStoreNames(), transferConfig.FSStoreNames(), store.MediaAssetTransferAttemptLimit,
	)
	if err != nil || !ok {
		t.Fatalf("preparing pending transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	objects := transferTestObjectStores(t, roots)
	targetRelPath := transfer.RelPath
	targetPath := filepath.Join(roots["archive"], filepath.FromSlash(targetRelPath))
	testutil.RequireNoError(t, objects["archive"].Put(context.Background(), targetRelPath, bytes.NewReader(video), int64(len(video)), sha256.Sum256(video), mediapublish.ObjectContentType(targetRelPath)), "putting pending derived destination")
	ownership, err := media.MarkMediaAssetTransferObjectOwned(context.Background(), transfer.CopyID, "archive", targetRelPath)
	if err != nil || ownership != store.TransferObjectOwned {
		t.Fatalf("recording pending destination ownership = (%d, %v), want owned", ownership, err)
	}
	subtitleRelPath, err := mediapath.SubtitleSibling(targetRelPath)
	testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
	subtitlePath := filepath.Join(roots["archive"], filepath.FromSlash(subtitleRelPath))
	testutil.RequireNoError(t, os.Mkdir(subtitlePath, 0o755), "os.Mkdir")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(subtitlePath, "keep.txt"), []byte("x"), 0o644), "making subtitle removal fail")
	ownership, err = media.MarkMediaAssetTransferObjectOwned(context.Background(), transfer.CopyID, "archive", subtitleRelPath)
	if err != nil || ownership != store.TransferObjectOwned {
		t.Fatalf("recording pending subtitle ownership = (%d, %v), want owned", ownership, err)
	}
	deleteConfig := transferTestConfig("test-store", "archive", "test-store")
	deleteMedia := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, deleteConfig))
	worker := DeleteReconcileWorker{
		DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: deleteMedia, StorageTransfer: deleteMedia, RecordingPurge: pgstore.NewRecordings(pool, "archive"), MediaPendingReclaim: media}, Storage: deleteMedia,
		Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["archive"], StorageObjects: testStorageObjects(t, "archive", roots["archive"]), MediaStoreName: "archive",
		DerivedObjects: objects["archive"], DerivedObjectStoreName: "archive",
	}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work with unremovable subtitle")
	if !fileExists(targetPath) {
		t.Fatal("canonical object was removed before subtitle cleanup succeeded")
	}
	var copyState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&copyState), "reading copy state after the first reclaim")
	if copyState != "pending" {
		t.Fatalf("pending copy state after interrupted reclaim = %q, want pending", copyState)
	}

	testutil.RequireNoError(t, os.RemoveAll(subtitlePath), "os.RemoveAll")
	testutil.RequireNoError(t, os.WriteFile(subtitlePath, []byte("WEBVTT\n\ncaption\n"), 0o644), "os.WriteFile")
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.DeleteReconcileArgs{}), "retrying DeleteReconcileWorker.Work")
	if fileExists(targetPath) || fileExists(subtitlePath) {
		t.Fatal("canonical or subtitle remains after pending reclaim retry")
	}
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&copyState), "reading copy state after the retry")
	if copyState != "deleted" {
		t.Errorf("pending copy state after successful reclaim = %q, want deleted", copyState)
	}
}

// TestStorageTransferWorkerResumesSavedProgressWithoutReadingSourcePrefix は、SHA-256 が既知のとき、
// 保存された先頭を読み直さずに続きだけを転送元から読むことを確かめる。転送元の先頭を変えておき、
// 結果が保存された先頭と元の続きになることで、先頭が読まれていないことを見る。
func TestStorageTransferWorkerResumesSavedProgressWithoutReadingSourcePrefix(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	content := []byte("known SHA-256 makes the saved destination prefix authoritative")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	expectedSHA := sha256.Sum256(content)
	testutil.MustExec(t, pool, "setting the expected logical sha256", `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, expectedSHA[:], assetID)
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10)
	if err != nil || !ok {
		t.Fatalf("preparing saved transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	prefix := content[:len(content)/2]
	saveTransferProgressForTest(t, media, transfer, roots["archive"], prefix)
	changedSource := bytes.Clone(content)
	changedSource[0] ^= 0xff
	sourcePath := filepath.Join(roots["test-store"], filepath.FromSlash(transfer.SourceRelPath))
	testutil.RequireNoError(t, os.WriteFile(sourcePath, changedSource, 0o644), "os.WriteFile")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}), "resuming known-SHA transfer")
	got, err := os.ReadFile(filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("resumed destination = %q, %v; want saved prefix plus original source suffix %q", got, err, content)
	}
}

// TestStorageTransferWorkerRestartsFromSourceWhenPreviousAttemptFailed は、前の試行が保存せずに抜けた（handed_off が偽）
// とき、世代を上げて転送元の先頭から取り直すことを確かめる。temp の先頭は壊しておき、続きから書いたら宛先の SHA-256 が合わない。
func TestStorageTransferWorkerRestartsFromSourceWhenPreviousAttemptFailed(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	content := []byte("an unsaved previous slice must not be continued from its temp")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	expectedSHA := sha256.Sum256(content)
	testutil.MustExec(t, pool, "setting the expected logical sha256", `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, expectedSHA[:], assetID)
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10)
	if err != nil || !ok {
		t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	leaseOwner := uuid.New()
	progress, acquired, err := media.AcquireMediaAssetTransferProgress(context.Background(), transfer.CopyID, leaseOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquiring progress = (%+v, %t, %v)", progress, acquired, err)
	}
	tempPath := transferTempPath(filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath)), assetID, transfer.CopyID, progress.WriterGeneration)
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(tempPath), 0o755), "os.MkdirAll")
	damaged := bytes.Clone(content[:len(content)/2])
	damaged[0] ^= 0xff
	testutil.RequireNoError(t, os.WriteFile(tempPath, damaged, 0o644), "os.WriteFile")
	testutil.RequireNoError(t, media.ReleaseMediaAssetTransferProgress(context.Background(), transfer.CopyID, leaseOwner), "media.ReleaseMediaAssetTransferProgress")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}), "Work after an unsaved slice")
	got, err := os.ReadFile(filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination = %q, %v; want the source from its start %q", got, err, content)
	}
	var attempts int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts WHERE copy_id = $1`, transfer.CopyID).Scan(&attempts), "counting transfer attempts")
	if attempts != 0 {
		t.Fatalf("transfer attempts after a lease-expired previous slice = %d, want 0 (an expired lease is not a failure)", attempts)
	}
	if fileExists(tempPath) {
		t.Fatalf("previous generation temp %q remains after the copy became active", tempPath)
	}
}

// TestStorageTransferWorkerRestartsWhenSavedTempLengthDiffers は、きれいな引き継ぎなのに保存された offset と temp が合わないとき、
// その試行を失敗として 1 回数え、次の試行が新しい世代で先頭から取り直して完了することを確かめる。
// 短い temp は続きから書くと穴が空き、宛先の SHA-256 が合わなくなる（SHA-256 が未知の資産では、長さの確認だけが防ぐ）。
func TestStorageTransferWorkerRestartsWhenSavedTempLengthDiffers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// shorten は保存のあとで temp を縮める量。extend は伸ばす量。remove は temp を消すこと。
		shorten, extend int
		remove          bool
	}{
		{name: "shorter temp than the saved offset", shorten: 3},
		{name: "longer temp than the saved offset", extend: 4},
		{name: "missing temp for the saved offset", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			content := []byte("a saved offset must match the temp length before the temp is continued")
			assetID := transferTestOriginal(t, pool, roots["test-store"], content)
			cfg := transferTestConfig("archive", "archive", "test-store")
			media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
			transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10)
			if err != nil || !ok {
				t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
			}
			saved := content[:len(content)/2]
			tempPath := saveTransferProgressForTest(t, media, transfer, roots["archive"], saved)
			switch {
			case tc.remove:
				testutil.RequireNoError(t, os.Remove(tempPath), "os.Remove")
			case tc.shorten > 0:
				testutil.RequireNoError(t, os.Truncate(tempPath, int64(len(saved)-tc.shorten)), "os.Truncate")
			default:
				extra, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_APPEND, 0)
				testutil.RequireNoError(t, err, "os.OpenFile")
				_, err = extra.Write(bytes.Repeat([]byte("j"), tc.extend))
				testutil.RequireNoError(t, err, "extra.Write")
				testutil.RequireNoError(t, extra.Close(), "extra.Close")
			}
			fingerprint, err := cfg.PolicyFingerprint()
			testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
			worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
			args := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}
			testutil.RequireNoError(t, worker.Work(context.Background(), args), "first Work with an inconsistent saved transfer")
			destination := filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath))
			if fileExists(destination) {
				t.Fatal("destination was published from an inconsistent saved transfer")
			}
			var attempts int32
			var lastError string
			testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT a.attempt_count, a.last_error
FROM media_asset_transfer_attempts a
WHERE a.copy_id = $1`, transferTestCopyID(t, pool, assetID, "archive")).Scan(&attempts, &lastError), "reading recorded transfer attempt")
			if attempts != 1 || !strings.Contains(lastError, "saved offset is") {
				t.Fatalf("recorded attempt = (%d, %q), want one failure naming the saved offset", attempts, lastError)
			}
			// 失敗した試行は世代を上げないので、次の試行の Acquire が世代を 1 つだけ上げ、不整合だった世代の temp をその試行が消す。
			if !tc.remove && !fileExists(tempPath) {
				t.Fatal("inconsistent temp was removed by the failed attempt; the next attempt should remove it")
			}
			testutil.RequireNoError(t, worker.Work(context.Background(), args), "second Work after the failure")
			got, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("destination after the restart = %q, %v; want %q", got, err, content)
			}
			if fileExists(tempPath) {
				t.Fatalf("inconsistent generation temp %q remains after the next attempt", tempPath)
			}
		})
	}
}

// TestStorageTransferWorkerSnoozesWhenOwnLeaseExpiredDuringSlice は、slice が止まったときに自分の lease が切れていて
// 進捗を保存できなかった場合、Work を成功で終えずに snooze することを確かめる。
func TestStorageTransferWorkerSnoozesWhenOwnLeaseExpiredDuringSlice(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	content := bytes.Repeat([]byte("slice-bytes-"), 65536)
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	restore := storageTransferSlice
	storageTransferSlice = 300 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	// slice の最中に lease を切る。取った直後（handed_off が偽で期限が先）の行だけを対象にする。
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			_, _ = pool.Exec(context.Background(), `
UPDATE media_asset_copy_transfer_progress SET lease_expires_at = now() - interval '1 second'
WHERE handed_off = false AND lease_expires_at > now()`)
		}
	}()
	err = worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint})
	close(stop)
	<-done
	if delay, ok := jobq.SnoozeDelay(err); !ok || delay != storageTransferLockSnooze {
		t.Fatalf("Work with an expired own lease = %v, want a snooze of %s", err, storageTransferLockSnooze)
	}
}

// TestStorageTransferWorkerSliceStopsAndResumesToCompletion は、slice の期限を超える転送が、1 回の Work で終わらずに
// 進捗を保存して snooze し、繰り返すと完了することを確かめる。
func TestStorageTransferWorkerSliceStopsAndResumesToCompletion(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	// 読みは 128 KiB ずつなので、slice の期限を超えるよう 128 KiB より十分大きくする。
	content := bytes.Repeat([]byte("slice-bytes-"), 65536)
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	restore := storageTransferSlice
	storageTransferSlice = 150 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	args := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}
	snoozes := 0
	var copyID int64
	for range 200 {
		err := worker.Work(context.Background(), args)
		if err == nil {
			break
		}
		delay, ok := jobq.SnoozeDelay(err)
		if !ok {
			t.Fatalf("Work returned %v; want a snooze until the copy is complete", err)
		}
		if delay != 0 {
			t.Fatalf("slice snooze delay = %s, want 0", delay)
		}
		snoozes++
		if copyID == 0 {
			copyID = transferTestCopyID(t, pool, assetID, "archive")
		}
		var handedOff bool
		var offset int64
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT handed_off, offset_bytes FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&handedOff, &offset), "reading saved progress after a slice")
		if !handedOff || offset <= 0 || offset >= int64(len(content)) {
			t.Fatalf("saved progress after slice %d = handed_off %t offset %d, want handed_off with 0 < offset < %d", snoozes, handedOff, offset, len(content))
		}
	}
	if snoozes == 0 {
		t.Fatal("transfer finished in one Work; want the slice limit to split it")
	}
	got, err := os.ReadFile(filepath.Join(roots["archive"], filepath.FromSlash(transferTestCopyRelPath(t, pool, assetID, "archive"))))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination after %d slices = %d bytes, %v; want %d bytes equal to the source", snoozes, len(got), err, len(content))
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
		testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
		worker := StorageTransferWorker{Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
		if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err == nil {
			t.Fatal("Work with unmounted target succeeded")
		}
		var copies, attempts int
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copies), "reading archive copy count")
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts), "counting transfer attempts")
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
		testutil.RequireNoError(t, os.WriteFile(filepath.Join(roots["archive"], mediapath.StoreMarkerFileName), []byte("wrong\n"), 0o600), "os.WriteFile")
		metrics.StorageTransferMountMismatches.DeleteLabelValues("archive")
		t.Cleanup(func() { metrics.StorageTransferMountMismatches.DeleteLabelValues("archive") })
		assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
		cfg := transferTestConfig("archive", "archive", "test-store")
		fingerprint, err := cfg.PolicyFingerprint()
		testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
		worker := StorageTransferWorker{Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
		if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err == nil {
			t.Fatal("Work with mismatched target marker succeeded")
		}
		var copies, attempts int
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copies), "reading archive copy count")
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts), "counting transfer attempts")
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
		testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
		worker := StorageTransferWorker{Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: map[string]string{"archive": roots["archive"]}, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
		testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}), "Work with unmounted source")
		var copies, attempts int
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&copies), "reading archive copy count")
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts), "counting transfer attempts")
		if copies != 0 || attempts != 0 {
			t.Fatalf("copies/attempts after missing source = %d/%d, want 0/0", copies, attempts)
		}
	})
}

func TestStorageTransferWorkerSnoozesWhenSourceLockIsBusyWithoutAttempt(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	lease, acquired, err := media.AcquireTransferSourceBandwidth(context.Background(), "test-store")
	if err != nil || !acquired {
		t.Fatalf("acquiring source lock = (%v, %v), want lease", acquired, err)
	}
	t.Cleanup(func() {
		if err := lease.Release(context.Background()); err != nil {
			t.Errorf("releasing source lock: %v", err)
		}
	})
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	err = worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint})
	if delay, ok := jobq.SnoozeDelay(err); !ok || delay != storageTransferLockSnooze {
		t.Fatalf("Work snooze = (%s, %v) with error %v, want %s", delay, ok, err, storageTransferLockSnooze)
	}
	var attempts int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts), "counting transfer attempts")
	if attempts != 0 {
		t.Fatalf("transfer attempts while source lock is busy = %d, want 0", attempts)
	}
}

func TestStorageTransferWorkerSnoozesDerivedWhenSourceLockIsBusyWithoutAttempt(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID,
		store.AssetKindThumbnail, nil, "sites/default/transfer/thumbnail.jpg", []byte("thumbnail source"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	lease, acquired, err := media.AcquireTransferSourceBandwidth(context.Background(), "test-store")
	if err != nil || !acquired {
		t.Fatalf("acquiring source lock = (%v, %v), want lease", acquired, err)
	}
	t.Cleanup(func() {
		if err := lease.Release(context.Background()); err != nil {
			t.Errorf("releasing source lock: %v", err)
		}
	})
	objects := transferTestObjectStores(t, roots)
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: objects, ScratchDir: t.TempDir()}
	err = worker.Work(context.Background(), jobs.StorageTransferArgs{
		MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint,
	})
	if delay, ok := jobq.SnoozeDelay(err); !ok || delay != storageTransferLockSnooze {
		t.Fatalf("derived transfer snooze = (%s, %v) with error %v, want %s", delay, ok, err, storageTransferLockSnooze)
	}
	var attempts int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts), "counting transfer attempts")
	if attempts != 0 {
		t.Fatalf("derived transfer attempts while source lock is busy = %d, want 0", attempts)
	}
	if _, err := objects["archive"].Head(context.Background(), "sites/default/transfer/thumbnail.jpg"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("derived target Head while source lock is busy = %v, want ErrNotFound", err)
	}
}

func TestPrepareMediaAssetTransferDoesNotReviveDeletingCopy(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("source"))
	dbtest.MediaAssetCopy(t, pool, assetID, func(p *dbtest.MediaAssetCopyParams) {
		p.Store = "archive"
		p.RelPath = "sites/default/transfer/source.ts"
		p.State = "deleting"
	})
	cfg := transferTestConfig("archive", "archive", "test-store")
	transfer, ok, err := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)).PrepareMediaAssetTransfer(
		context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10,
	)
	if err != nil || ok {
		t.Fatalf("preparing while destination is deleting = (%+v, %t, %v), want no reservation", transfer, ok, err)
	}
	state := transferTestCopyState(t, pool, assetID)
	if state != "deleting" {
		t.Fatalf("destination state after prepare = %q, want deleting", state)
	}
}

func TestStorageTransferWorkerReportsOriginalMismatchWithoutDeletingSource(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	actual := []byte("badbytes")
	assetID := transferTestOriginal(t, pool, roots["test-store"], actual)
	expected := sha256.Sum256([]byte("goodbyte"))
	testutil.MustExec(t, pool, "setting expected source checksum", `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, expected[:], assetID)
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	worker := StorageTransferWorker{Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{
		MediaAssetID: assetID,
		Store:        "archive", PolicyFingerprint: fingerprint,
	}), "Work mismatch")
	if !hasCurrentCopyInStore(t, pool, assetID, "test-store") {
		t.Fatal("current original copy in the source store is missing after mismatch")
	}
	if got, err := os.ReadFile(filepath.Join(roots["test-store"], "sites/default/transfer/source.ts")); err != nil || !bytes.Equal(got, actual) {
		t.Fatalf("source after mismatch = %q, %v; want %q", got, err, actual)
	}
	if fileExists(filepath.Join(roots["archive"], "sites/default/transfer/source.ts")) {
		t.Fatal("mismatched source was published to destination")
	}
	var attempts int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT attempt.attempt_count FROM media_asset_transfer_attempts attempt JOIN media_asset_copies copy ON copy.id = attempt.copy_id WHERE copy.media_asset_id = $1 AND copy.store = 'archive'`, assetID).Scan(&attempts), "reading failed attempt")
	if attempts != 1 {
		t.Fatalf("transfer attempt count = %d, want 1", attempts)
	}
}

func TestStorageTransferWorkerRetiresCorruptDerivedSourceOnlyWhenOriginalCanRebuild(t *testing.T) {
	for _, tc := range []struct {
		name            string
		originalPresent bool
		encoded         bool
		boundStore      string
		wantSource      string
		wantTarget      string
		wantAttempts    int
	}{
		{name: "present original permits rebuild", originalPresent: true, wantSource: "deleted", wantTarget: "deleted"},
		{name: "present original permits encoded rebuild", originalPresent: true, encoded: true, wantSource: "deleted", wantTarget: "deleted"},
		{name: "source in a store other than the Media binding is marked deleted", originalPresent: true, boundStore: "archive", wantSource: "deleted", wantTarget: "deleted"},
		{name: "missing original reports without retiring source", wantSource: "active", wantTarget: "pending", wantAttempts: 1},
		{name: "missing original keeps encoded source", encoded: true, wantSource: "active", wantTarget: "pending", wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			recordingID := dbtest.Recording(t, pool)
			if tc.originalPresent {
				seedOriginalAsset(t, pool, roots["test-store"], recordingID, "sites/default/transfer/original.ts", []byte("original source"))
			}
			relPath := "sites/default/transfer/thumbnail.jpg"
			kind := store.AssetKindThumbnail
			var profile *string
			content := []byte("thumbnail bytes")
			if tc.encoded {
				relPath = "sites/default/transfer/encoded.mp4"
				kind = store.AssetKindEncoded
				web := "web"
				profile = &web
				content = []byte("encoded bytes")
			}
			assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, kind, profile, relPath, content)
			sourcePath := filepath.Join(roots["test-store"], filepath.FromSlash(relPath))
			sidecarPath := ""
			if tc.encoded {
				sidecarRelPath, err := mediapath.SubtitleSibling(relPath)
				testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
				sidecarPath = filepath.Join(roots["test-store"], filepath.FromSlash(sidecarRelPath))
				testutil.RequireNoError(t, os.WriteFile(sidecarPath, []byte("WEBVTT\n\ncaptions\n"), 0o644), "os.WriteFile")
				markRecordingUntilEncoded(t, pool, recordingID, []string{"web"})
			}
			wrongSHA := sha256.Sum256([]byte("different derived bytes"))
			testutil.MustExec(t, pool, "setting mismatched logical checksum", "UPDATE media_assets SET sha256 = $1 WHERE id = $2", wrongSHA[:], assetID)
			cfg := transferTestConfig("archive", "archive", "test-store")
			fingerprint, err := cfg.PolicyFingerprint()
			testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
			retiredBefore := promtestutil.ToFloat64(metrics.StorageTransferCorruptSourcesRetired)
			bound := tc.boundStore
			if bound == "" {
				bound = "test-store"
			}
			worker := StorageTransferWorker{Storage: pgstore.NewMediaInStore(pool, bound, mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
			testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{
				MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint,
			}), "Work with corrupt sole derived copy")

			var sourceState, targetState string
			testutil.RequireNoError(t, pool.QueryRow(context.Background(),
				"SELECT source.state, target.state FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id AND target.generation = source.generation WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
				assetID, "test-store", "archive").Scan(&sourceState, &targetState), "reading copy states after derived source mismatch")
			if sourceState != tc.wantSource || targetState != tc.wantTarget {
				t.Fatalf("source/target copy states = %q/%q, want %q/%q", sourceState, targetState, tc.wantSource, tc.wantTarget)
			}
			var attempts int
			testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts attempt JOIN media_asset_copies copy ON copy.id = attempt.copy_id WHERE copy.media_asset_id = $1`, assetID).Scan(&attempts), "counting transfer attempts of the derived copy")
			if attempts != tc.wantAttempts {
				t.Fatalf("transfer attempt rows = %d, want %d", attempts, tc.wantAttempts)
			}
			if relPath := transferTestLatestCopyRelPath(t, pool, assetID, "archive"); relPath != "" && fileExists(filepath.Join(roots["archive"], filepath.FromSlash(relPath))) {
				t.Fatal("mismatched derived source was published to the destination")
			}
			retired := tc.wantSource == "deleted"
			if got := fileExists(sourcePath); got == retired {
				t.Fatalf("source file exists = %t after retirement = %t, want the file removed exactly when the copy is retired", got, retired)
			}
			if sidecarPath != "" {
				if got := fileExists(sidecarPath); got == retired {
					t.Fatalf("subtitle sidecar exists = %t after retirement = %t, want the sidecar removed together with the encoded file", got, retired)
				}
			}
			wantRetired := 0.0
			if retired {
				wantRetired = 1
			}
			if got := promtestutil.ToFloat64(metrics.StorageTransferCorruptSourcesRetired) - retiredBefore; got != wantRetired {
				t.Fatalf("corrupt source retirement counter increased by %v, want %v", got, wantRetired)
			}
			if tc.encoded {
				var missingProfile bool
				testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM unencoded_desired_profiles WHERE recording_id = $1 AND profile = 'web')`, recordingID).Scan(&missingProfile), "reading whether the web profile is still desired")
				if missingProfile != retired {
					t.Fatalf("unencoded_desired_profiles has the retired profile = %t, want %t", missingProfile, retired)
				}
			}
		})
	}
}

func TestStorageTransferWorkerUsesObjectStoreForDerivedCopies(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	relPath := "sites/default/transfer/thumbnail.jpg"
	content := []byte("derived bytes copied through objstore")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindThumbnail, nil, relPath, content)
	expectedSHA := sha256.Sum256(content)
	testutil.MustExec(t, pool, "setting logical checksum", "UPDATE media_assets SET sha256 = $1 WHERE id = $2", expectedSHA[:], assetID)
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	worker := StorageTransferWorker{
		Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots,
		ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir(),
	}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}), "Work")
	destination := filepath.Join(roots["archive"], filepath.FromSlash(transferTestCopyRelPath(t, pool, assetID, "archive")))
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("derived transfer destination = %q, %v; want %q", got, err, content)
	}
	if fileExists(transferTempPath(destination, assetID, transferTestCopyID(t, pool, assetID, "archive"), 1)) {
		t.Fatal("derived transfer left an FS transfer temp beside the object key")
	}
	var sourceState, targetState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		"SELECT source.state, target.state FROM media_asset_copies source JOIN media_asset_copies target ON target.media_asset_id = source.media_asset_id AND target.generation = source.generation WHERE source.media_asset_id = $1 AND source.store = $2 AND target.store = $3",
		assetID, "test-store", "archive").Scan(&sourceState, &targetState), "reading copy states")
	if sourceState != "active" || targetState != "active" {
		t.Fatalf("source/target states = %q/%q, want active/active", sourceState, targetState)
	}
}

func TestStorageTransferWorkerRejectsOldFingerprintAndNewPolicyKeepsAValidCopy(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive", "cold")
	content := []byte("asset after policy change")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	oldConfig := transferTestConfig("archive", "archive", "cold", "test-store")
	newConfig := transferTestConfig("cold", "archive", "cold", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, newConfig))
	oldFingerprint, err := oldConfig.PolicyFingerprint()
	testutil.RequireNoError(t, err, "oldConfig.PolicyFingerprint")
	newFingerprint, err := newConfig.PolicyFingerprint()
	testutil.RequireNoError(t, err, "newConfig.PolicyFingerprint")
	oldJob := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: oldFingerprint}
	worker := StorageTransferWorker{Storage: media, Config: newConfig, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	if err := worker.Work(context.Background(), oldJob); !errors.Is(err, jobq.ErrDiscard) {
		t.Fatalf("old policy job error = %v, want jobq.ErrDiscard", err)
	}
	var archiveCopies int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&archiveCopies), "reading archive copy count")
	if archiveCopies != 0 {
		t.Fatalf("old policy created %d archive copies, want 0", archiveCopies)
	}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{
		MediaAssetID: assetID, Store: "cold", PolicyFingerprint: newFingerprint,
	}), "copying under new policy")
	testutil.MustExec(t, pool, "aging the cold copy past the overlap", `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'cold'`, assetID)
	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), time.Now().Add(-newConfig.Transfer.Overlap), 10)
	testutil.RequireNoError(t, err, "listing stale copies after policy switch")
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
		DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: media, StorageTransfer: media, RecordingPurge: pgstore.NewRecordings(pool, "test-store")}, Storage: media,
		Breakers: pgstore.NewBreakers(pool),
		MediaDir: roots["test-store"], StorageObjects: testStorageObjects(t, "test-store", roots["test-store"]), MediaStoreName: "test-store", TransferOverlap: time.Hour,
	}
	testutil.RequireNoError(t, deleteWorker.Work(context.Background(), jobs.DeleteReconcileArgs{}), "deleting stale source copy")
	if fileExists(filepath.Join(roots["test-store"], "sites/default/transfer/source.ts")) {
		t.Fatal("stale source file remains after delete reconcile")
	}
	sourceState := storeCopyState(t, pool, "test-store", assetID)
	if sourceState != "deleted" {
		t.Fatalf("stale source copy state = %q, want deleted", sourceState)
	}
	var activeStores []string
	rows, err := pool.Query(context.Background(), `SELECT store FROM media_asset_copies WHERE media_asset_id = $1 AND state = 'active' ORDER BY store`, assetID)
	testutil.RequireNoError(t, err, "pool.Query")
	defer rows.Close()
	for rows.Next() {
		var name string
		testutil.RequireNoError(t, rows.Scan(&name), "rows.Scan")
		activeStores = append(activeStores, name)
	}
	testutil.RequireNoError(t, rows.Err(), "rows.Err")
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
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	enqueuer := &jobqfaketest.RecordingEnqueuer{}
	reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg}
	testutil.RequireNoError(t, reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "reconcile Work")
	args := enqueuer.CallsOfKind("storage_transfer")
	if len(args) != 1 {
		t.Fatalf("enqueued storage_transfer jobs = %d, want 1", len(args))
	}
	got, ok := args[0].Args.(jobs.StorageTransferArgs)
	if !ok {
		t.Fatalf("enqueued arg type = %T, want jobs.StorageTransferArgs", args[0].Args)
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

func TestStorageTransferReconcileWarnsForActiveDerivedCopyInNonDerivedRemote(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/old-s3.mp4", []byte("encoded"))
	transferTestAddCopy(t, pool, assetID, "s3-a", "sites/default/transfer/old-s3.mp4")
	cfg := config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"test-store": {Kind: "fs"},
			"s3-a":       {Kind: "s3", Bucket: "old"},
			"s3-b":       {Kind: "s3", Bucket: "new"},
		},
		Originals: []config.OriginalStoreRule{{Store: "test-store"}},
		Derived:   "s3-b",
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	metrics.StorageDerivedCopiesOutsideDerivedS3.Set(0)
	t.Cleanup(func() { metrics.StorageDerivedCopiesOutsideDerivedS3.Set(0) })

	reconcile := StorageTransferReconcileWorker{
		Enqueuer: &jobqfaketest.RecordingEnqueuer{}, Storage: pgstore.NewMediaUnboundWithPolicy(pool, mustStoragePolicy(t, cfg)), Config: cfg,
	}
	testutil.RequireNoError(t, reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "storage transfer reconcile")
	if got := promtestutil.ToFloat64(metrics.StorageDerivedCopiesOutsideDerivedS3); got != 1 {
		t.Fatalf("active derived copies outside current remote store = %v, want 1", got)
	}
	if !strings.Contains(logs.String(), "active derived copies remain in a non-derived remote store") || !strings.Contains(logs.String(), "store=s3-a") {
		t.Fatalf("reconcile warning = %q, want warning naming s3-a", logs.String())
	}
}

func TestStorageTransferReconcileDoesNotWarnWithoutActiveDerivedCopyInNonDerivedRemote(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/deleted-s3.mp4", []byte("encoded"))
	transferTestAddCopy(t, pool, assetID, "s3-a", "sites/default/transfer/deleted-s3.mp4")
	transferTestAddCopy(t, pool, assetID, "s3-b", "sites/default/transfer/current-s3.mp4")
	testutil.MustExec(t, pool, "retiring old S3 copy", `UPDATE media_asset_copies SET state = 'deleted' WHERE media_asset_id = $1 AND store = 's3-a'`, assetID)
	cfg := config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"test-store": {Kind: "fs"},
			"s3-a":       {Kind: "s3", Bucket: "old"},
			"s3-b":       {Kind: "s3", Bucket: "new"},
		},
		Originals: []config.OriginalStoreRule{{Store: "test-store"}},
		Derived:   "s3-b",
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	metrics.StorageDerivedCopiesOutsideDerivedS3.Set(3)
	t.Cleanup(func() { metrics.StorageDerivedCopiesOutsideDerivedS3.Set(0) })

	reconcile := StorageTransferReconcileWorker{
		Enqueuer: &jobqfaketest.RecordingEnqueuer{}, Storage: pgstore.NewMediaUnboundWithPolicy(pool, mustStoragePolicy(t, cfg)), Config: cfg,
	}
	testutil.RequireNoError(t, reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "storage transfer reconcile")
	if got := promtestutil.ToFloat64(metrics.StorageDerivedCopiesOutsideDerivedS3); got != 0 {
		t.Fatalf("active derived copies outside current remote store = %v, want 0", got)
	}
	if strings.Contains(logs.String(), "active derived copies remain in a non-derived remote store") {
		t.Fatalf("reconcile warned without an active stale copy: %q", logs.String())
	}
}

func TestStorageTransferReconcileReportsExhaustionAndClearsObsoleteAttempts(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive", "cold")
	assetID := transferTestOriginal(t, pool, roots["test-store"], []byte("attempted transfer"))
	archiveConfig := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, archiveConfig))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", archiveConfig.FSStoreNames(), archiveConfig.FSStoreNames(), archiveConfig.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing failed transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	for range 9 {
		testutil.RequireNoError(t, media.RecordMediaAssetTransferFailure(context.Background(), transfer.CopyID, "destination unavailable"), "media.RecordMediaAssetTransferFailure")
	}
	metrics.StorageTransferWaitingAssets.Set(0)
	metrics.StorageTransferWaitingBytes.Set(0)
	metrics.StorageTransferExhaustedAssets.Set(0)
	t.Cleanup(func() {
		metrics.StorageTransferWaitingAssets.Set(0)
		metrics.StorageTransferWaitingBytes.Set(0)
		metrics.StorageTransferExhaustedAssets.Set(0)
	})
	nearLimitEnqueuer := &jobqfaketest.RecordingEnqueuer{}
	nearLimitPass := StorageTransferReconcileWorker{Enqueuer: nearLimitEnqueuer, Storage: media, Config: archiveConfig}
	testutil.RequireNoError(t, nearLimitPass.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "nearLimitPass.Work")
	if got := len(nearLimitEnqueuer.CallsOfKind("storage_transfer")); got != 1 {
		t.Fatalf("jobs after 9 failed attempts = %d, want 1", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferWaitingAssets); got != 1 {
		t.Fatalf("waiting assets after 9 failed attempts = %v, want 1", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferExhaustedAssets); got != 0 {
		t.Fatalf("exhausted assets after 9 failed attempts = %v, want 0", got)
	}
	testutil.RequireNoError(t, media.RecordMediaAssetTransferFailure(context.Background(), transfer.CopyID, "destination unavailable"), "recording the tenth failure")
	firstEnqueuer := &jobqfaketest.RecordingEnqueuer{}
	firstPass := StorageTransferReconcileWorker{Enqueuer: firstEnqueuer, Storage: media, Config: archiveConfig}
	testutil.RequireNoError(t, firstPass.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "firstPass.Work")
	if got := len(firstEnqueuer.CallsOfKind("storage_transfer")); got != 0 {
		t.Fatalf("jobs after exhausted attempts = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferWaitingAssets); got != 0 {
		t.Fatalf("waiting assets after exhausted attempts = %v, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferExhaustedAssets); got != 1 {
		t.Fatalf("exhausted assets = %v, want 1", got)
	}

	coldConfig := transferTestConfig("cold", "archive", "cold", "test-store")
	secondEnqueuer := &jobqfaketest.RecordingEnqueuer{}
	coldMedia := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, coldConfig))
	secondPass := StorageTransferReconcileWorker{Enqueuer: secondEnqueuer, Storage: coldMedia, Config: coldConfig}
	testutil.RequireNoError(t, secondPass.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "secondPass.Work")
	if got := len(secondEnqueuer.CallsOfKind("storage_transfer")); got != 1 {
		t.Fatalf("jobs after desired store changes = %d, want 1", got)
	}
	var attempts int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_attempts`).Scan(&attempts), "counting transfer attempts")
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
	t.Parallel()
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
				testutil.MustExec(t, pool, "marking the recording deleted", `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID)
			},
			policy:  func() config.StorageConfig { return transferTestConfig("archive", "archive", "test-store") },
			wantOne: true,
		},
		{
			name: "new generation",
			change: func(t *testing.T, pool *pgxpool.Pool, _, assetID int64) {
				t.Helper()
				testutil.MustExec(t, pool, "bumping the derived generation", `UPDATE media_assets SET derived_generation = 1 WHERE id = $1`, assetID)
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
			media := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, initialPolicy))
			transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", initialPolicy.FSStoreNames(), initialPolicy.FSStoreNames(), initialPolicy.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
			if err != nil || !ok {
				t.Fatalf("preparing pending copy = (%+v, %t, %v)", transfer, ok, err)
			}
			if tc.change != nil {
				tc.change(t, pool, recordingID, assetID)
			}
			policy := tc.policy()
			policyMedia := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, policy))
			copies, err := policyMedia.ListObsoletePendingMediaAssetTransferCopies(context.Background(), 10)
			testutil.RequireNoError(t, err, "listing obsolete pending copies")
			if gotOne := len(copies) == 1; gotOne != tc.wantOne {
				t.Fatalf("obsolete pending copy count = %d, want one=%t (rows: %+v)", len(copies), tc.wantOne, copies)
			}
		})
	}
}

func TestStorageTransferPendingReclaimRechecksAfterRecordingRestore(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/restored.mp4", []byte("encoded"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, cfg))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing pending copy = (%+v, %t, %v)", transfer, ok, err)
	}
	testutil.MustExec(t, pool, "marking the recording deleted", `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID)
	candidates, err := media.ListObsoletePendingMediaAssetTransferCopies(context.Background(), 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("obsolete candidates before restore = %+v, %v; want one", candidates, err)
	}
	testutil.MustExec(t, pool, "restoring the recording", `UPDATE recordings SET deleted_at = NULL WHERE id = $1`, recordingID)
	called := false
	reclaimed, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(
		context.Background(), candidates[0],
		func([]string) error { called = true; return nil },
	)
	testutil.RequireNoError(t, err, "reclaiming a restored pending copy")
	if reclaimed || called {
		t.Fatalf("reclaim after restore = (reclaimed %t, delete called %t), want no-op", reclaimed, called)
	}
	var state string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&state), "reading copy state after recording restore")
	if state != "pending" {
		t.Fatalf("pending copy after restore = %q, want pending", state)
	}
}

func prepareUntilEncodedPendingOriginal(t *testing.T, pool *pgxpool.Pool, roots map[string]string, recordingID int64) (int64, store.MediaAssetTransfer, config.StorageConfig, int64) {
	t.Helper()
	originalRelPath := "sites/default/retention/pending-original.ts"
	originalID := seedOriginalAsset(t, pool, roots["test-store"], recordingID, originalRelPath, []byte("source-original"))
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, cfg))
	transfer, ok, err := media.PrepareMediaAssetTransfer(
		context.Background(), originalID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), store.MediaAssetTransferAttemptLimit,
	)
	if err != nil || !ok {
		t.Fatalf("preparing pending original transfer = (%+v, %t, %v)", transfer, ok, err)
	}

	profile := "web"
	addDerived := func(kind, relPath string, profile *string, content []byte) int64 {
		t.Helper()
		assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, kind, profile, relPath, content)
		archivePath := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
		testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(archivePath), 0o755), "os.MkdirAll")
		testutil.RequireNoError(t, os.WriteFile(archivePath, content, 0o644), "os.WriteFile")
		transferTestAddCopy(t, pool, assetID, "archive", relPath)
		return assetID
	}
	addDerived(store.AssetKindEncoded, "sites/default/retention/encoded.mp4", &profile, []byte("encoded"))
	addDerived(store.AssetKindThumbnail, "sites/default/retention/thumbnail.jpg", nil, []byte("thumbnail"))
	seekTilesID := addDerived(store.AssetKindSeekTiles, "sites/default/retention/tiles.jpg", nil, []byte("tiles"))
	markRecordingUntilEncoded(t, pool, recordingID, []string{profile})
	var eligible bool
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM until_encoded_deletable_originals('archive') WHERE asset_id = $1)`, originalID).Scan(&eligible), "checking desired-store until_encoded eligibility")
	if !eligible {
		t.Fatal("until_encoded fixture does not qualify for original deletion")
	}
	return originalID, transfer, cfg, transferTestCopyID(t, pool, seekTilesID, "archive")
}

func TestDeleteReconcileReclaimsPendingOriginalAfterUntilEncoded(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	originalID, transfer, cfg, _ := prepareUntilEncodedPendingOriginal(t, pool, roots, recordingID)
	targetPath := filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755), "os.MkdirAll")
	// 実際の試行と同じく、進捗の行を作ってから世代の名前で temp を書く。回収は行の世代で temp を見つける。
	progress, acquired, err := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, cfg)).AcquireMediaAssetTransferProgress(context.Background(), transfer.CopyID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquiring transfer progress = (%+v, %t, %v)", progress, acquired, err)
	}
	tempPath := transferTempPath(targetPath, transfer.MediaAssetID, transfer.CopyID, progress.WriterGeneration)
	testutil.RequireNoError(t, os.WriteFile(tempPath, []byte("source-original"), 0o644), "os.WriteFile")
	testutil.RequireNoError(t, os.Link(tempPath, targetPath), "linking published pending destination to transfer temp")

	objects := transferTestObjectStores(t, roots)
	derivedMedia := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, cfg))
	derivedDeleteStores, storeErr := store.NewDerivedObjectDeleteStores(derivedMedia, derivedMedia, derivedMedia)
	testutil.RequireNoError(t, storeErr, "creating derived object delete stores")
	newWorker := func(storeName string) *DeleteReconcileWorker {
		media := pgstore.NewMediaInStore(pool, storeName, mustStoragePolicy(t, cfg))
		worker := &DeleteReconcileWorker{
			DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: media, StorageTransfer: media, RecordingPurge: pgstore.NewRecordings(pool, storeName), DerivedObjectDeleteStores: derivedDeleteStores}, Storage: media,
			Breakers: pgstore.NewBreakers(pool),
			MediaDir: roots[storeName], StorageObjects: testStorageObjects(t, storeName, roots[storeName]), MediaStoreName: storeName, TrashRetention: time.Hour,
			DerivedObjects: objects["archive"], DerivedObjectStoreName: "archive",
		}
		if storeName == "archive" {
			worker.MediaPendingReclaim = media
		}
		return worker
	}
	testutil.RequireNoError(t, newWorker("test-store").Work(context.Background(), jobs.DeleteReconcileArgs{}), "deleting source original after until_encoded")
	if fileExists(filepath.Join(roots["test-store"], filepath.FromSlash("sites/default/retention/pending-original.ts"))) {
		t.Fatal("until_encoded source original remains after deletion reconcile")
	}
	sourceState := storeCopyState(t, pool, "test-store", originalID)
	if sourceState != "deleted" {
		t.Fatalf("until_encoded source copy state = %q, want deleted", sourceState)
	}
	if !fileExists(targetPath) {
		t.Fatal("fixture destination should remain before its pending reservation is reclaimed")
	}
	testutil.RequireNoError(t, newWorker("archive").Work(context.Background(), jobs.DeleteReconcileArgs{}), "reclaiming until_encoded pending destination")
	if fileExists(targetPath) || fileExists(tempPath) {
		t.Fatal("pending destination or transfer temp remains after until_encoded made its transfer obsolete")
	}
	var targetState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&targetState), "reading target copy state after reclaim")
	if targetState != "deleted" {
		t.Fatalf("until_encoded pending copy state = %q, want deleted", targetState)
	}
}

func TestStorageTransferPendingReclaimRechecksUntilEncodedEligibility(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	_, transfer, cfg, seekTilesCopyID := prepareUntilEncodedPendingOriginal(t, pool, roots, recordingID)
	media := pgstore.NewMediaInStore(pool, "archive", mustStoragePolicy(t, cfg))
	candidates, err := media.ListObsoletePendingMediaAssetTransferCopies(
		context.Background(), 10,
	)
	if err != nil || len(candidates) != 1 || candidates[0].CopyID != transfer.CopyID {
		t.Fatalf("until_encoded obsolete candidates = %+v, %v; want pending original copy %d", candidates, err, transfer.CopyID)
	}
	testutil.MustExec(t, pool, "making until_encoded eligibility false", `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, seekTilesCopyID)
	var eligible bool
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM until_encoded_deletable_originals('archive') WHERE asset_id = $1)`, transfer.MediaAssetID).Scan(&eligible), "reading until_encoded deletability")
	if eligible {
		t.Fatal("fixture still qualifies for until_encoded after desired seek tiles were marked missing")
	}
	called := false
	reclaimed, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(
		context.Background(), candidates[0],
		func([]string) error { called = true; return nil },
	)
	testutil.RequireNoError(t, err, "rechecking pending original retention")
	if reclaimed || called {
		t.Fatalf("reclaim after until_encoded eligibility changed = (reclaimed %t, delete called %t), want no-op", reclaimed, called)
	}
	var state string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE id = $1`, transfer.CopyID).Scan(&state), "reading copy state after the delete pass")
	if state != "pending" {
		t.Fatalf("pending copy after retention recheck = %q, want pending", state)
	}
}

func TestUntilEncodedOriginalWaitsForPresentDerivedCopiesInDesiredStore(t *testing.T) {
	t.Parallel()
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
		testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM until_encoded_deletable_originals('archive') WHERE asset_id = $1`, originalID).Scan(&count), "querying desired-store retention predicate")
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
	for _, assetID := range []int64{encodedID, thumbnailID, seekTilesID} {
		copyID := transferTestCopyID(t, pool, assetID, "archive")
		testutil.MustExec(t, pool, "marking desired copy missing", `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, copyID)
		assertOriginalEligibility(false)
		testutil.MustExec(t, pool, "removing the missing-media marker", `DELETE FROM missing_media_assets WHERE media_asset_copy_id = $1`, copyID)
		assertOriginalEligibility(true)
	}

	cfg := transferTestConfig("archive", "archive", "test-store")
	candidates, err := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)).ListMediaAssetTransferCandidates(
		context.Background(), 10, 100,
	)
	testutil.RequireNoError(t, err, "listing transfer candidates after until_encoded eligibility")
	for _, candidate := range candidates {
		if candidate.MediaAssetID == originalID {
			t.Fatal("until_encoded original remained a transfer candidate after the desired derived copies were present")
		}
	}
}

func TestUntilEncodedOriginalStallsWhenEncodedExistsOnlyInOldS3AfterDerivedStoreSwitch(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "sites/default/s3-switch/original.ts", []byte("original"))
	profile := "h264"
	encodedID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, "sites/default/s3-switch/encoded.mp4", []byte("encoded"))
	thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, "sites/default/s3-switch/thumbnail.jpg", []byte("thumb"))
	seekTilesID := seedSeekTilesAsset(t, pool, mediaDir, recordingID, "sites/default/s3-switch/tiles.jpg")
	markRecordingUntilEncoded(t, pool, recordingID, []string{profile})
	derivedIDs := []int64{encodedID, thumbnailID, seekTilesID}
	testutil.MustExec(t, pool, "retiring fixture FS copies", `
UPDATE media_asset_copies
SET state = 'deleted', deleted_at = now()
WHERE media_asset_id = ANY($1::bigint[]) AND store = 'test-store'`, derivedIDs)
	for _, assetID := range derivedIDs {
		transferTestAddCopy(t, pool, assetID, "s3-a", fmt.Sprintf("sites/default/s3-switch/s3-a-%d", assetID))
	}

	var activeElsewhere int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT count(*) FROM media_asset_copies
WHERE media_asset_id = ANY($1::bigint[]) AND state = 'active' AND store <> 's3-a'`, derivedIDs).Scan(&activeElsewhere), "checking active derived copies outside s3-a")
	if activeElsewhere != 0 {
		t.Fatalf("active derived copies outside s3-a = %d, want 0", activeElsewhere)
	}

	cfg := config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"test-store": {Kind: "fs"},
			"s3-a":       {Kind: "s3", Bucket: "old-bucket"},
			"s3-b":       {Kind: "s3", Bucket: "new-bucket"},
		},
		Originals: []config.OriginalStoreRule{{Store: "test-store"}},
		Derived:   "s3-b",
		Transfer:  config.StorageTransferConfig{Overlap: time.Hour},
	}
	var deletable bool
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT EXISTS (
  SELECT 1 FROM until_encoded_deletable_originals('s3-b') WHERE asset_id = $1
)`, originalID).Scan(&deletable), "querying until_encoded deletion eligibility for s3-b")
	if deletable {
		t.Fatal("until_encoded original became deletable although derived copies exist only in s3-a")
	}

	missing, err := pgstore.NewMediaUnboundWithPolicy(pool, mustStoragePolicy(t, cfg)).ListMissingEncodeProfiles(context.Background(), store.MissingEncodeProfilesParams{
		RecordingID:   &recordingID,
		KnownProfiles: []string{profile},
		CutProfiles:   []string{},
		AttemptLimit:  10,
		RowLimit:      10,
	})
	testutil.RequireNoError(t, err, "listing missing encode profiles for s3-b")
	for _, row := range missing {
		if row.RecordingID == recordingID && row.Profile == profile {
			t.Fatalf("old-S3 encoded profile was queued for re-encode: %+v", row)
		}
	}

	policy := mustStoragePolicy(t, cfg)
	candidates, err := pgstore.NewMediaUnboundWithPolicy(pool, policy).ListMediaAssetTransferCandidates(
		context.Background(), 10, 100,
	)
	testutil.RequireNoError(t, err, "listing derived transfer candidates for s3-b")
	for _, candidate := range candidates {
		if candidate.MediaAssetID == encodedID && candidate.SourceStore != "" {
			t.Fatalf("encoded asset in s3-a has transfer source %q, want no FS source", candidate.SourceStore)
		}
	}
}

// 判定の条件（希望先の現在コピーの存在・overlap の境界・実体無し）は SQL 関数のテスト TestMediaAssetCopyPrunable が固定する。
// ここは Go が渡す overlap の cutoff と、候補の列挙・削除印・削除印の後の再列挙という配線だけを見る。
func TestStorageTransferDeletionWiresOverlapCutoff(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/delete/source.ts"
	})
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))

	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/delete/source.ts")
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), time.Now().Add(-time.Hour), 10); err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates before overlap = %+v, %v; want none", candidates, err)
	}
	testutil.MustExec(t, pool, "aging the archive copy past the overlap", `UPDATE media_asset_copies SET created_at = now() - interval '2 hours' WHERE media_asset_id = $1 AND store = 'archive'`, assetID)
	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), time.Now(), 10)
	testutil.RequireNoError(t, err, "media.ListMediaAssetCopiesToDelete")
	if len(candidates) != 1 || candidates[0].Store != "test-store" {
		t.Fatalf("stale copy candidates = %+v, want only test-store", candidates)
	}
	path, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), candidates[0], time.Now())
	if err != nil || !marked || path != "sites/default/delete/source.ts" {
		t.Fatalf("marking stale copy = (%q, %t, %v), want path, true, nil", path, marked, err)
	}
	if candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), time.Now(), 10); err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates after stale copy marked = %+v, %v; want none (a deleting copy is picked up again by the pending path)", candidates, err)
	}
}

func TestStorageTransferDeletionKeepsCopiesWhenPolicyHasNoDesiredStore(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/no-desired/source.ts"
	})
	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/no-desired/source.ts")
	transferTestAddCopy(t, pool, assetID, "cold", "sites/default/no-desired/source.ts")
	// overlap を外し、ゼロ方針で削除されないことを検査する。
	testutil.MustExec(t, pool, "backdating copies past the overlap", `UPDATE media_asset_copies SET created_at = now() - interval '2 hours' WHERE media_asset_id = $1 AND store IN ('archive', 'cold')`, assetID)
	media := pgstore.NewMediaInStoreWithoutPolicy(pool, "archive")

	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), time.Now(), 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates with no desired store = %+v, %v; want none", candidates, err)
	}
	candidate := store.MediaAssetCopyDeleteCandidate{
		CopyID:       transferTestCopyID(t, pool, assetID, "archive"),
		MediaAssetID: assetID, Store: "archive", RelPath: "sites/default/no-desired/source.ts",
	}
	if _, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), candidate, time.Now()); err != nil || marked {
		t.Fatalf("marking copy with no desired store = %v, %v; want false, nil", marked, err)
	}
	var activeCopies int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT count(*) FROM media_asset_copies
WHERE media_asset_id = $1 AND state = 'active' AND store IN ('archive', 'cold')`, assetID).Scan(&activeCopies), "counting surviving copies")
	if activeCopies != 2 {
		t.Fatalf("active copies after no-policy deletion = %d, want both copies retained", activeCopies)
	}
}

func TestStorageTransferDeletionSkipsWhenPlacementArmIsDisabled(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/placement-disabled/source.ts"
	})
	cfg := transferTestConfig("archive", "archive")
	if mustStoragePolicy(t, cfg).PlacementEnabled() {
		t.Fatal("placement arm is enabled with only one transfer destination")
	}
	if got := mustStoragePolicy(t, cfg).DerivedStore(); got != "archive" {
		t.Fatalf("base policy derived store = %q, want archive", got)
	}
	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/placement-disabled/source.ts")
	transferTestAddCopy(t, pool, assetID, "cold", "sites/default/placement-disabled/source.ts")
	// desired コピーを overlap より古くし、配置削除の無効化だけが削除を止めている状態にする。
	testutil.MustExec(t, pool, "aging the desired archive copy past the overlap", `UPDATE media_asset_copies SET created_at = now() - interval '2 hours' WHERE media_asset_id = $1 AND store = 'archive'`, assetID)
	media := pgstore.NewMediaInStore(pool, "cold", mustStoragePolicy(t, cfg))
	candidate := store.MediaAssetCopyDeleteCandidate{
		CopyID:       transferTestCopyID(t, pool, assetID, "cold"),
		MediaAssetID: assetID,
		Store:        "cold",
		RelPath:      "sites/default/placement-disabled/source.ts",
	}

	candidates, err := media.ListMediaAssetCopiesToDelete(context.Background(), time.Now(), 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("deletion candidates with placement disabled = %+v, %v; want none", candidates, err)
	}
	if _, marked, err := media.MarkMediaAssetCopyDeleting(context.Background(), candidate, time.Now()); err != nil || marked {
		t.Fatalf("marking copy with placement disabled = %v, %v; want false, nil", marked, err)
	}
	var state string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'cold'`, assetID).Scan(&state), "reading cold copy state")
	if state != "active" {
		t.Fatalf("stale copy state with placement disabled = %q, want active", state)
	}
}

func TestStorageTransferDeletionSerializesPolicySwitches(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/policy-switch/source.ts"
	})
	oldConfig := transferTestConfig("archive", "archive", "cold", "test-store")
	newConfig := transferTestConfig("cold", "archive", "cold", "test-store")
	oldRules := mustStoragePolicy(t, oldConfig).OriginalRules()
	newRules := mustStoragePolicy(t, newConfig).OriginalRules()
	oldPolicy := store.NewStoragePolicy(oldRules, nil, "archive").WithPlacementEnabled(true)
	newPolicy := store.NewStoragePolicy(newRules, nil, "cold").WithPlacementEnabled(true)
	oldMedia := pgstore.NewMediaInStore(pool, "test-store", oldPolicy)
	newMedia := pgstore.NewMediaInStore(pool, "test-store", newPolicy)
	transferTestAddCopy(t, pool, assetID, "archive", "sites/default/policy-switch/source.ts")
	transferTestAddCopy(t, pool, assetID, "cold", "sites/default/policy-switch/source.ts")
	testutil.MustExec(t, pool, "retiring the test-store copy", `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = 'test-store'`, assetID)
	// 作成時刻を十分過去に固定し、DB と Go の時計差を除いて削除印の直列化を検証する。
	testutil.MustExec(t, pool, "backdating the copy created_at", `UPDATE media_asset_copies SET created_at = '2000-01-01T00:00:00Z'::timestamptz WHERE media_asset_id = $1`, assetID)
	archive := store.MediaAssetCopyDeleteCandidate{CopyID: transferTestCopyID(t, pool, assetID, "archive"), MediaAssetID: assetID, Store: "archive", RelPath: "sites/default/policy-switch/source.ts"}
	cold := store.MediaAssetCopyDeleteCandidate{CopyID: transferTestCopyID(t, pool, assetID, "cold"), MediaAssetID: assetID, Store: "cold", RelPath: "sites/default/policy-switch/source.ts"}

	testutil.MustExec(t, pool, "creating delete-race trigger function", `
CREATE FUNCTION public.pause_storage_copy_delete_for_test() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IS DISTINCT FROM NEW.state AND NEW.state = 'deleting' THEN
        PERFORM pg_sleep(0.3);
    END IF;
    RETURN NEW;
END
$$`)
	testutil.MustExec(t, pool, "creating delete-race trigger", `
CREATE TRIGGER pause_storage_copy_delete_for_test
BEFORE UPDATE OF state ON media_asset_copies
FOR EACH ROW EXECUTE FUNCTION public.pause_storage_copy_delete_for_test()`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS pause_storage_copy_delete_for_test ON media_asset_copies`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS public.pause_storage_copy_delete_for_test()`)
	})

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
		_, marked, err := oldMedia.MarkMediaAssetCopyDeleting(context.Background(), cold, time.Now())
		results <- struct {
			marked bool
			err    error
		}{marked, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		_, marked, err := newMedia.MarkMediaAssetCopyDeleting(context.Background(), archive, time.Now())
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
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND state = 'active'`, assetID).Scan(&activeCount), "counting active copies")
	if activeCount != 1 {
		t.Fatalf("active copies after concurrent policy switch = %d, want 1", activeCount)
	}
}

func mustStoragePolicy(t *testing.T, cfg config.StorageConfig) store.StoragePolicy {
	t.Helper()
	policy, err := cfg.BuildStorePolicy()
	testutil.RequireNoError(t, err, "cfg.BuildStorePolicy")
	return policy
}

// 論理資産の SHA-256 が既知の派生物は、source の Get をそのまま Put に流し、scratch に複製しない。
// ScratchDir を作れない場所にしても転送が成功することで確かめる。
func TestStorageTransferWorkerStreamsKnownSHADerivedWithoutScratch(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/streamed.mp4"
	content := []byte("encoded bytes streamed without a scratch copy")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, content)
	sum := sha256.Sum256(content)
	testutil.MustExec(t, pool, "setting the expected logical sha256", "UPDATE media_assets SET sha256 = $1 WHERE id = $2", sum[:], assetID)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	testutil.RequireNoError(t, os.WriteFile(blocker, nil, 0o644), "os.WriteFile")
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	worker := StorageTransferWorker{
		Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots,
		ObjectStores: transferTestObjectStores(t, roots), ScratchDir: filepath.Join(blocker, "scratch"),
	}
	testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}), "Work")
	got, err := os.ReadFile(filepath.Join(roots["archive"], filepath.FromSlash(transferTestCopyRelPath(t, pool, assetID, "archive"))))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination = %q, %v; want the source bytes", got, err)
	}
	state := transferTestCopyState(t, pool, assetID)
	if state != "active" {
		t.Fatalf("destination copy state = %q, want active", state)
	}
}

// 既知の SHA-256 または宣言サイズと違う source は、Put が何も公開せず、source 不一致として扱われる。
func TestStorageTransferWorkerStreamedDerivedSourceMismatchPublishesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		declare    func(content []byte) (size int64, sum [sha256.Size]byte)
		wantReason string
	}{
		{name: "different SHA-256", declare: func(c []byte) (int64, [sha256.Size]byte) {
			return int64(len(c)), sha256.Sum256([]byte("other bytes"))
		}},
		{name: "source shorter than declared size", declare: func(c []byte) (int64, [sha256.Size]byte) {
			return int64(len(c)) + 5, sha256.Sum256(c)
		}},
		{name: "source longer than declared size", declare: func(c []byte) (int64, [sha256.Size]byte) {
			return int64(len(c)) - 5, sha256.Sum256(c)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			recordingID := dbtest.Recording(t, pool)
			seedOriginalAsset(t, pool, roots["test-store"], recordingID, "sites/default/transfer/original.ts", []byte("original source"))
			profile := "web"
			relPath := "sites/default/transfer/mismatch.mp4"
			content := []byte("encoded bytes that disagree with the logical row")
			assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, content)
			size, sum := tc.declare(content)
			testutil.MustExec(t, pool, "setting the logical sha256 and size", "UPDATE media_assets SET sha256 = $1, size_bytes = $2 WHERE id = $3", sum[:], size, assetID)
			cfg := transferTestConfig("archive", "archive", "test-store")
			fingerprint, err := cfg.PolicyFingerprint()
			testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
			worker := StorageTransferWorker{
				Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: roots,
				ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir(),
			}
			testutil.RequireNoError(t, worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}), "Work")
			targetRelPath := transferTestLatestCopyRelPath(t, pool, assetID, "archive")
			if targetRelPath != "" && fileExists(filepath.Join(roots["archive"], filepath.FromSlash(targetRelPath))) {
				t.Fatal("a mismatched derived source created the destination key")
			}
			if files := transferTestPayloadFiles(t, roots["archive"]); len(files) != 0 {
				t.Fatalf("mismatched derived source left destination objects: %v", files)
			}
			// 原本が残っているので、不一致の source は再構築のために退役する（handleTransferMismatch）。
			sourceState := storeCopyState(t, pool, "test-store", assetID)
			if sourceState != "deleted" {
				t.Fatalf("mismatched source copy state = %q, want deleted (treated as a source mismatch)", sourceState)
			}
		})
	}
}

// TestTransferTempOrphanCleanupFollowsProgressGeneration は、転送 temp の孤児回収が、コピーの進捗の世代で判定することを確かめる。
// 進捗が世代 3 を指すとき、古い世代 1・2 の temp は（mtime が古ければ）消え、今の世代 3 の temp は残る。
// 進捗の無い temp は、mtime の猶予の間は残り、猶予を過ぎたら消える。
func TestTransferTempOrphanCleanupFollowsProgressGeneration(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	content := []byte("progress generation decides which transfer temp is still owned")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	media := pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg))
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), 10)
	if err != nil || !ok {
		t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	// 世代を 3 まで進める。各試行は lease を切ってから次が取る（保存はしないので、次は世代を上げる）。
	ctx := context.Background()
	for want := int64(1); want <= 3; want++ {
		owner := uuid.New()
		progress, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, transfer.CopyID, owner, time.Minute)
		if err != nil || !acquired || progress.WriterGeneration != want {
			t.Fatalf("acquire #%d = (%+v, %t, %v), want generation %d", want, progress, acquired, err, want)
		}
		testutil.RequireNoError(t, media.ReleaseMediaAssetTransferProgress(ctx, transfer.CopyID, owner), "media.ReleaseMediaAssetTransferProgress")
	}
	const noProgressCopyID = 987654
	type candidate struct {
		relPath string
		aged    bool
		keep    bool
	}
	var candidates []candidate
	for _, c := range []struct {
		copyID int64
		gen    int64
		aged   bool
		keep   bool
	}{
		{copyID: transfer.CopyID, gen: 1, aged: true, keep: false},
		{copyID: transfer.CopyID, gen: 2, aged: true, keep: false},
		{copyID: transfer.CopyID, gen: 3, aged: true, keep: true},
		{copyID: noProgressCopyID, gen: 1, aged: true, keep: false},
		{copyID: noProgressCopyID, gen: 2, aged: false, keep: true},
	} {
		relPath := filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(transfer.RelPath)),
			fmt.Sprintf("%s%d-%d-g%d", mediapath.StorageTransferTempFilePrefix, assetID, c.copyID, c.gen)))
		path := filepath.Join(roots["archive"], filepath.FromSlash(relPath))
		testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "os.MkdirAll")
		testutil.RequireNoError(t, os.WriteFile(path, []byte("partial"), 0o644), "os.WriteFile")
		if c.aged {
			old := time.Now().Add(-2 * time.Hour)
			testutil.RequireNoError(t, os.Chtimes(path, old, old), "os.Chtimes")
		}
		candidates = append(candidates, candidate{relPath: relPath, aged: c.aged, keep: c.keep})
	}
	worker := DeleteReconcileWorker{
		Storage: media, MediaDir: roots["archive"], MediaStoreName: "archive",
		StorageObjects: testStorageObjects(t, "archive", roots["archive"]),
	}
	for _, c := range candidates {
		worker.deleteOrphanFile(c.relPath, time.Hour)
		path := filepath.Join(roots["archive"], filepath.FromSlash(c.relPath))
		if got := fileExists(path); got != c.keep {
			t.Errorf("%s exists = %t after orphan cleanup, want %t (aged=%t)", c.relPath, got, c.keep, c.aged)
		}
	}
}
