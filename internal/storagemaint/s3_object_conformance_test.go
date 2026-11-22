//go:build s3conformance

package storagemaint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mediapublish"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestS3DerivedObjectPublicationAndRecovery は、実際の S3 API の実装（CI では RustFS）に対して、
// 派生物の公開・回収を測る。DB の振る舞いは object_store_test.go が fsstore で測る。
// ここでは FS への公開から始め、S3 転送と既存キーの回収に必要な S3 の応答
// （条件付き書き込み・multipart・HEAD の SHA-256・List）を確かめる。
//
//nolint:funlen // S3 の公開と回収の順序を 1 本の時系列で検査する。分けると前後関係が追えなくなる。
func TestS3DerivedObjectPublicationAndRecovery(t *testing.T) {
	ctx := t.Context()
	endpoint := os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")
	bucket := os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("ROKUBAN_S3_CONFORMANCE_ENDPOINT and ROKUBAN_S3_CONFORMANCE_BUCKET are required")
	}
	region := os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION")
	if region == "" {
		region = "us-east-1"
	}
	objects, err := s3store.New(ctx, s3store.Config{Endpoint: endpoint, Region: region, Bucket: bucket})
	testutil.RequireNoError(t, err, "creating S3 object store")
	env := newObjectEnvWith(t, objects)
	pool := env.pool
	media := env.media
	root := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(root, dbtest.StoreName), "writing source store marker")
	sourceFS, err := fsstore.New(root)
	testutil.RequireNoError(t, err, "creating source FS object store")
	fsMedia := pgstore.NewMediaInStore(pool, dbtest.StoreName, store.NewStoragePolicy(nil, []string{dbtest.StoreName}, objectStoreName))
	prefix := fmt.Sprintf("thumbnails/s3-conformance-%d", time.Now().UnixNano())
	encodedKey := fmt.Sprintf("encoded/s3-conformance-%d.mp4", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range []string{prefix + "-pending.jpg", prefix + "-unknown.jpg"} {
			if err := objects.Delete(context.Background(), key); err != nil {
				t.Errorf("deleting S3 test key %q: %v", key, err)
			}
		}
	})

	t.Run("initial encoded output becomes active in FS before transfer to S3", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		profile := "h264"
		dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) { p.EncodeProfiles = []string{profile} })
		if _, err := pgstoretest.Media(pool).Begin(ctx, recordingID, profile, jobs.EncodeAttemptLimit); err != nil {
			t.Fatalf("seeding running encode attempt: %v", err)
		}
		payload := []byte("first encoded object bytes")
		scratchPath := t.TempDir() + "/encoded.mp4"
		writeFile(t, scratchPath, payload)
		pending := store.PendingMediaAssetCopy{
			RecordingID: recordingID, Kind: store.AssetKindEncoded, Profile: &profile, RelPath: encodedKey, ReserveNewGeneration: true,
		}
		size, published, err := mediapublish.PublishGeneratedObject(ctx, sourceFS, fsMedia, pending, scratchPath, "video/mp4", false, nil,
			func(size int64, sum [sha256.Size]byte) (bool, error) {
				return fsMedia.PublishEncoded(ctx, store.EncodedPublishInput{
					RecordingID: recordingID, Profile: profile, Attempt: 1, RelPath: encodedKey, SizeBytes: size,
					SHA256: sum[:], PendingCopy: true,
				}, func() error { return nil })
			})
		testutil.RequireNoError(t, err, "publishing initial FS encoded asset")
		if !published || size != int64(len(payload)) {
			t.Fatalf("publish result = (%d, %t), want (%d, true)", size, published, len(payload))
		}
		var assetID int64
		var state string
		testutil.RequireNoError(t, pool.QueryRow(ctx, `
SELECT media_asset_id, state FROM media_asset_copies WHERE store = $1 AND rel_path = $2
`, dbtest.StoreName, encodedKey).Scan(&assetID, &state), "reading committed FS copy")
		if state != "active" {
			t.Fatalf("committed FS encoded copy state = %q, want active", state)
		}
		cfg := config.StorageConfig{
			Stores: map[string]config.StoreConfig{
				dbtest.StoreName: {Kind: "fs"},
				objectStoreName:  {Kind: "s3", Bucket: bucket},
			},
			Originals: []config.OriginalStoreRule{{Store: dbtest.StoreName}},
			Derived:   objectStoreName,
		}
		fingerprint, err := cfg.PolicyFingerprint()
		testutil.RequireNoError(t, err, "fingerprinting transfer policy")
		worker := StorageTransferWorker{
			Storage:      pgstore.NewMediaInStore(pool, dbtest.StoreName, mustStoragePolicy(t, cfg)),
			Config:       cfg,
			Roots:        map[string]string{dbtest.StoreName: root},
			ObjectStores: map[string]objstore.Store{dbtest.StoreName: sourceFS, objectStoreName: objects},
			ScratchDir:   t.TempDir(),
		}
		t.Cleanup(func() {
			if key := transferTestLatestCopyRelPath(t, pool, assetID, objectStoreName); key != "" {
				if err := objects.Delete(context.Background(), key); err != nil {
					t.Errorf("deleting S3 transfer key %q: %v", key, err)
				}
			}
		})
		args := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: objectStoreName, SourceStore: dbtest.StoreName, PolicyFingerprint: fingerprint}
		testutil.RequireNoError(t, worker.Work(ctx, args), "transferring initial encoded asset")
		targetKey := transferTestCopyRelPath(t, pool, assetID, objectStoreName)
		if targetKey == encodedKey {
			t.Fatalf("S3 transfer reused the FS key %q", targetKey)
		}
		if got := env.copyState(t, targetKey); got != "active" {
			t.Fatalf("transferred encoded copy state = %q, want active", got)
		}
		object, err := objects.Head(ctx, targetKey)
		testutil.RequireNoError(t, err, "heading transferred object")
		if object.Size != int64(len(payload)) || object.SHA256 != fmt.Sprintf("%x", sha256.Sum256(payload)) {
			t.Fatalf("published object metadata = (size %d, sha256 %q), want (%d, %x)", object.Size, object.SHA256, len(payload), sha256.Sum256(payload))
		}
	})

	t.Run("pending commits before FS Put and active follows Put", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		originalID := dbtest.MediaAsset(t, pool, recordingID)
		key := prefix + "-active.jpg"
		payload := []byte("published thumbnail bytes")
		scratchPath := t.TempDir() + "/thumbnail.jpg"
		writeFile(t, scratchPath, payload)
		observing := &pendingObservedStore{Store: sourceFS, t: t, pool: pool, storeName: dbtest.StoreName}
		pending := store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key}
		size, published, err := mediapublish.PublishGeneratedObject(ctx, observing, fsMedia, pending, scratchPath, "image/jpeg", false, nil,
			func(size int64, sum [sha256.Size]byte) (bool, error) {
				return fsMedia.PublishThumbnail(ctx, store.ThumbnailPublishInput{
					RecordingID: recordingID, RelPath: key, SizeBytes: size, InputAssetID: originalID,
					SHA256: sum[:], PendingCopy: true,
				}, func() error { return nil })
			})
		testutil.RequireNoError(t, err, "publishing FS thumbnail")
		if !observing.sawPending {
			t.Fatal("PUT did not observe a committed pending copy row")
		}
		if !published || size != int64(len(payload)) {
			t.Fatalf("publish result = (%d, %t), want (%d, true)", size, published, len(payload))
		}
		var state string
		testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, dbtest.StoreName, key).Scan(&state), "reading committed FS thumbnail copy")
		if state != "active" {
			t.Fatalf("published copy state = %q, want active", state)
		}
	})

	t.Run("uploaded pending copy is reclaimed after a worker crash", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		key := prefix + "-pending.jpg"
		payload := []byte("uploaded before simulated process death")
		digest := sha256.Sum256(payload)
		pending := store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key, SizeBytes: int64(len(payload))}
		testutil.RequireNoError(t, media.PreparePendingMediaAssetCopy(ctx, pending), "committing pending copy")
		testutil.RequireNoError(t, objects.Put(ctx, key, strings.NewReader(string(payload)), int64(len(payload)), digest, "image/jpeg"), "uploading object before simulated process death")
		testutil.MustExec(t, pool, "aging pending copy", `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1 AND rel_path = $2`, objectStoreName, key)
		reclaimer := env.deleter(t, 100)
		testutil.RequireNoError(t, reclaimer.workObjectStore(ctx, reclaimer.settings()), "reclaiming stale pending copy")
		if _, err := objects.Head(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("reclaimed S3 key HEAD error = %v, want ErrNotFound", err)
		}
		if got := env.copyState(t, key); got != "deleted" {
			t.Fatalf("reclaimed copy state = %q, want deleted", got)
		}
	})

	t.Run("DB-less S3 keys are reported and retained", func(t *testing.T) {
		key := prefix + "-unknown.jpg"
		payload := []byte("unknown keys are not automatically deleted")
		digest := sha256.Sum256(payload)
		testutil.RequireNoError(t, objects.Put(ctx, key, strings.NewReader(string(payload)), int64(len(payload)), digest, "image/jpeg"), "uploading DB-less object")
		cleanup := env.deleter(t, 100)
		testutil.RequireNoError(t, cleanup.reconcileObjectOrphans(ctx, -time.Second, 0), "reconciling unknown S3 key")
		if _, err := objects.Head(ctx, key); err != nil {
			t.Fatalf("DB-less S3 key was removed instead of retained: %v", err)
		}
		aged, err := media.ListAgedOrphanFiles(ctx, 0)
		testutil.RequireNoError(t, err, "listing reported DB-less key")
		if !containsString(aged, key) {
			t.Fatalf("DB-less key %q is absent from aged orphan report %v", key, aged)
		}
	})

	t.Run("FS derived copy transfers to S3 across slices", func(t *testing.T) {
		// 転送元の FS 置き場の worker が S3 へ直接送る。S3 のパートは 5 MiB 以上なので、帯域の上限と短い slice で
		// 複数の slice に分け、途中の保存から続けて完了することを確かめる。
		root := t.TempDir()
		testutil.RequireNoError(t, mediapath.WriteStoreMarker(root, dbtest.StoreName), "writing source store marker")
		recordingID := dbtest.Recording(t, pool)
		profile := "h264"
		relPath := fmt.Sprintf("encoded/s3-fs-transfer-%d.mp4", time.Now().UnixNano())
		content := bytes.Repeat([]byte("fs-to-s3-transfer-"), 3<<20)
		assetID := seedEncodedOrThumbnailAsset(t, pool, root, recordingID, store.AssetKindEncoded, &profile, relPath, content)
		targetRelPath := ""
		t.Cleanup(func() {
			if targetRelPath == "" {
				targetRelPath = transferTestLatestCopyRelPath(t, pool, assetID, objectStoreName)
			}
			if targetRelPath == "" {
				return
			}
			if err := objects.Delete(context.Background(), targetRelPath); err != nil {
				t.Errorf("deleting S3 transfer key %q: %v", targetRelPath, err)
			}
		})
		cfg := config.StorageConfig{
			Stores: map[string]config.StoreConfig{
				dbtest.StoreName: {Kind: "fs"},
				objectStoreName:  {Kind: "s3", Bucket: bucket},
			},
			Originals: []config.OriginalStoreRule{{Store: dbtest.StoreName}},
			Derived:   objectStoreName,
			Transfer: config.StorageTransferConfig{
				Overlap:   2 * time.Hour,
				Bandwidth: map[string]string{dbtest.StoreName: "10MiBps"},
			},
		}
		fingerprint, err := cfg.PolicyFingerprint()
		testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
		restoreSlice := storageTransferSlice
		storageTransferSlice = time.Second
		t.Cleanup(func() { storageTransferSlice = restoreSlice })
		sourceFS, err := fsstore.New(root)
		testutil.RequireNoError(t, err, "fsstore.New")
		worker := StorageTransferWorker{
			Storage: pgstore.NewMediaInStore(pool, dbtest.StoreName, mustStoragePolicy(t, cfg)), Config: cfg,
			Roots:        map[string]string{dbtest.StoreName: root},
			ObjectStores: map[string]objstore.Store{dbtest.StoreName: sourceFS, objectStoreName: objects},
			ScratchDir:   t.TempDir(),
		}
		args := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: objectStoreName, SourceStore: dbtest.StoreName, PolicyFingerprint: fingerprint}
		snoozes := 0
		for range 300 {
			err := worker.Work(ctx, args)
			if err == nil {
				break
			}
			if delay, ok := jobq.SnoozeDelay(err); !ok || delay != 0 {
				t.Fatalf("Work returned %v; want a slice snooze with delay 0", err)
			}
			snoozes++
		}
		if snoozes == 0 {
			t.Fatal("transfer finished in one slice; the test needs several slices to exercise resumption")
		}
		targetRelPath = transferTestCopyRelPath(t, pool, assetID, objectStoreName)
		if targetRelPath == relPath {
			t.Fatalf("S3 copy rel_path = %q, want a fresh path distinct from the source", targetRelPath)
		}
		if got := env.copyState(t, targetRelPath); got != "active" {
			t.Fatalf("S3 copy state = %q, want active", got)
		}
		object, err := objects.Head(ctx, targetRelPath)
		testutil.RequireNoError(t, err, "heading transferred S3 object")
		if object.Size != int64(len(content)) || object.SHA256 != fmt.Sprintf("%x", sha256.Sum256(content)) {
			t.Fatalf("transferred S3 metadata = (size %d, sha256 %q), want (%d, %x)", object.Size, object.SHA256, len(content), sha256.Sum256(content))
		}

		// 完了した upload の送信状態は、進捗の行が消えるときに掃除待ちへ移る。その行が実 RustFS の Abort で
		// gone になり、掃除で消えることを確かめる。完了直後に行があることを先に見て、空虚な成功にしない。
		abandonedOnS3 := func() int {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_transfer_abandoned_uploads WHERE store = $1`, objectStoreName).Scan(&n); err != nil {
				t.Fatalf("counting abandoned uploads on %s: %v", objectStoreName, err)
			}
			return n
		}
		if n := abandonedOnS3(); n < 1 {
			t.Fatalf("abandoned uploads on %s after completion = %d, want at least 1 (the completed upload)", objectStoreName, n)
		}
		worker.abortAbandonedUploads(ctx, objectStoreName)
		if n := abandonedOnS3(); n != 0 {
			t.Fatalf("abandoned uploads on %s after cleanup = %d, want 0 (RustFS must answer NoSuchUpload for a completed upload)", objectStoreName, n)
		}
	})
}

type pendingObservedStore struct {
	objstore.Store
	t          *testing.T
	pool       *pgxpool.Pool
	storeName  string
	sawPending bool
}

func (s *pendingObservedStore) Put(ctx context.Context, key string, src io.Reader, size int64, checksum [32]byte, contentType string) error {
	s.t.Helper()
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, s.storeName, key).Scan(&state); err != nil {
		return fmt.Errorf("checking copy state immediately before PUT: %w", err)
	}
	if state != "pending" {
		return fmt.Errorf("copy state immediately before PUT = %q, want pending", state)
	}
	s.sawPending = true
	return s.Store.Put(ctx, key, src, size, checksum, contentType)
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
