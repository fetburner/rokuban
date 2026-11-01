//go:build s3conformance

package worker

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
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

// TestS3DerivedObjectPublicationAndRecovery は、実際の S3 API の実装（CI では MinIO）に対して、
// 派生物の公開・回収・入力の読み出しを測る。DB の振る舞いは object_store_test.go が fsstore で測るので、
// ここは S3 の応答（条件付き書き込み・multipart・HEAD の SHA-256・List）に依存する部分に絞る。
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
	if err != nil {
		t.Fatalf("creating S3 object store: %v", err)
	}
	env := newObjectEnvWith(t, objects)
	pool := env.pool
	media := env.media
	prefix := fmt.Sprintf("thumbnails/s3-conformance-%d", time.Now().UnixNano())
	encodedKey := fmt.Sprintf("encoded/s3-conformance-%d.mp4", time.Now().UnixNano())
	thumbnailInputKey := fmt.Sprintf("encoded/s3-conformance-%d-thumbnail-source.mp4", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range []string{prefix + "-active.jpg", prefix + "-pending.jpg", prefix + "-unknown.jpg", encodedKey, thumbnailInputKey} {
			if err := objects.Delete(context.Background(), key); err != nil {
				t.Errorf("deleting S3 test key %q: %v", key, err)
			}
		}
	})

	t.Run("initial encoded output activates its pending S3 copy", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		profile := "h264"
		seedEncodePolicy(t, pool, recordingID, []string{profile})
		if _, err := sqlcgen.New(pool).CreateRecordingEncodeAttemptRunning(ctx, sqlcgen.CreateRecordingEncodeAttemptRunningParams{
			RecordingID: recordingID, Profile: profile,
		}); err != nil {
			t.Fatalf("seeding running encode attempt: %v", err)
		}
		payload := []byte("first encoded object bytes")
		scratchPath := t.TempDir() + "/encoded.mp4"
		writeFile(t, scratchPath, payload)
		pending := store.PendingMediaAssetCopy{
			RecordingID: recordingID, Kind: store.AssetKindEncoded, Profile: &profile, RelPath: encodedKey, ReserveNewGeneration: true,
		}
		size, published, err := publishGeneratedObject(ctx, objects, media, pending, scratchPath, "video/mp4", false, nil,
			func(size int64, sum [sha256.Size]byte) (bool, error) {
				return media.PublishEncoded(ctx, store.EncodedPublishInput{
					RecordingID: recordingID, Profile: profile, Attempt: 1, RelPath: encodedKey, SizeBytes: size,
					SHA256: sum[:], PendingCopy: true,
				}, func() error { return nil })
			})
		if err != nil {
			t.Fatalf("publishing initial S3 encoded asset: %v", err)
		}
		if !published || size != int64(len(payload)) {
			t.Fatalf("publish result = (%d, %t), want (%d, true)", size, published, len(payload))
		}
		if got := env.copyState(t, encodedKey); got != "active" {
			t.Fatalf("committed encoded copy state = %q, want active", got)
		}
		object, err := objects.Head(ctx, encodedKey)
		if err != nil {
			t.Fatalf("heading published object: %v", err)
		}
		if object.Size != int64(len(payload)) || object.SHA256 != fmt.Sprintf("%x", sha256.Sum256(payload)) {
			t.Fatalf("published object metadata = (size %d, sha256 %q), want (%d, %x)", object.Size, object.SHA256, len(payload), sha256.Sum256(payload))
		}
	})

	t.Run("retry after an uploaded pending copy reserves a new generation", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		mediaDir := t.TempDir()
		if err := mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName); err != nil {
			t.Fatalf("writing local media store marker: %v", err)
		}
		sourceRel := fmt.Sprintf("s3-retry-%d/source.m2ts", time.Now().UnixNano())
		prepareEncodeInput(t, pool, recordingID, mediaDir, sourceRel, []string{"h264"}, []byte("original source"))
		initialKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 0, false)
		if err != nil {
			t.Fatalf("building initial encoded key: %v", err)
		}
		retryKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 1, false)
		if err != nil {
			t.Fatalf("building retry encoded key: %v", err)
		}
		t.Cleanup(func() {
			for _, key := range []string{initialKey, retryKey} {
				if err := objects.Delete(context.Background(), key); err != nil {
					t.Errorf("deleting S3 test key %q: %v", key, err)
				}
			}
		})

		worker := newH264Worker(pool, mediaDir, t.TempDir(), installCountingFFmpeg(t))
		worker.MediaStoreName = dbtest.StoreName
		worker.Objects = objects
		worker.ObjectPublisher = media
		worker.Media = media
		worker.Attempts = media

		originalHook := beforeEncodeCommit
		t.Cleanup(func() { beforeEncodeCommit = originalHook })
		failOnce := true
		beforeEncodeCommit = func(string) error {
			if failOnce {
				failOnce = false
				return errors.New("simulated stop after PUT and before DB publication")
			}
			return nil
		}
		args := jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}
		if err := worker.Work(ctx, args); err == nil {
			t.Fatal("first encode succeeded, want a failure after the S3 PUT")
		}
		firstBytes := env.readObject(t, initialKey)
		if err := worker.Work(ctx, args); err != nil {
			t.Fatalf("retrying encode with a new generation: %v", err)
		}
		secondBytes := env.readObject(t, retryKey)
		if bytes.Equal(firstBytes, secondBytes) {
			t.Fatalf("two ffmpeg attempts produced identical fixtures %q; want differing bytes", firstBytes)
		}
		if got := env.copyState(t, retryKey); got != "active" {
			t.Fatalf("retry copy state = %q, want active", got)
		}
		if got := env.copyState(t, initialKey); got != "pending" {
			t.Errorf("first copy state = %q, want pending", got)
		}
	})

	t.Run("pending commits before PUT and active follows PUT", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		originalID := dbtest.MediaAsset(t, pool, recordingID)
		key := prefix + "-active.jpg"
		payload := []byte("published thumbnail bytes")
		scratchPath := t.TempDir() + "/thumbnail.jpg"
		writeFile(t, scratchPath, payload)
		observing := &pendingObservedStore{Store: objects, t: t, pool: pool, storeName: objectStoreName}
		pending := store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key}
		size, published, err := publishGeneratedObject(ctx, observing, media, pending, scratchPath, "image/jpeg", false, nil,
			func(size int64, sum [sha256.Size]byte) (bool, error) {
				return media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
					RecordingID: recordingID, RelPath: key, SizeBytes: size, InputAssetID: originalID,
					SHA256: sum[:], PendingCopy: true,
				}, func() error { return nil })
			})
		if err != nil {
			t.Fatalf("publishing S3 thumbnail: %v", err)
		}
		if !observing.sawPending {
			t.Fatal("PUT did not observe a committed pending copy row")
		}
		if !published || size != int64(len(payload)) {
			t.Fatalf("publish result = (%d, %t), want (%d, true)", size, published, len(payload))
		}
		if got := env.copyState(t, key); got != "active" {
			t.Fatalf("published copy state = %q, want active", got)
		}
	})

	t.Run("uploaded pending copy is reclaimed after a worker crash", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		key := prefix + "-pending.jpg"
		payload := []byte("uploaded before simulated process death")
		digest := sha256.Sum256(payload)
		pending := store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key, SizeBytes: int64(len(payload))}
		if err := media.PreparePendingMediaAssetCopy(ctx, pending); err != nil {
			t.Fatalf("committing pending copy: %v", err)
		}
		if err := objects.Put(ctx, key, strings.NewReader(string(payload)), int64(len(payload)), digest, "image/jpeg"); err != nil {
			t.Fatalf("uploading object before simulated process death: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1 AND rel_path = $2`, objectStoreName, key); err != nil {
			t.Fatalf("aging pending copy: %v", err)
		}
		reclaimer := env.deleter(100)
		if err := reclaimer.workObjectStore(ctx, reclaimer.settings()); err != nil {
			t.Fatalf("reclaiming stale pending copy: %v", err)
		}
		if _, err := objects.Head(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("reclaimed S3 key HEAD error = %v, want ErrNotFound", err)
		}
		if got := env.copyState(t, key); got != "deleted" {
			t.Fatalf("reclaimed copy state = %q, want deleted", got)
		}
	})

	t.Run("thumbnail reselection reads encoded input from S3", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		profile := "h264"
		encodedPayload := []byte("remote encoded thumbnail input")
		encodedDigest := sha256.Sum256(encodedPayload)
		encodedID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
			p.Kind, p.Profile, p.RelPath, p.SizeBytes = store.AssetKindEncoded, &profile, thumbnailInputKey, int64(len(encodedPayload))
		})
		if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = $2 WHERE media_asset_id = $1`, encodedID, objectStoreName); err != nil {
			t.Fatalf("placing the encoded copy in the S3 store: %v", err)
		}
		if err := objects.Put(ctx, thumbnailInputKey, strings.NewReader(string(encodedPayload)), int64(len(encodedPayload)), encodedDigest, "video/mp4"); err != nil {
			t.Fatalf("uploading encoded thumbnail input: %v", err)
		}

		oldThumbnailKey := prefix + "-reselect.jpg"
		newThumbnailKey := fmt.Sprintf("thumbnails/%d.g1.jpg", recordingID)
		t.Cleanup(func() {
			for _, key := range []string{oldThumbnailKey, newThumbnailKey} {
				if err := objects.Delete(context.Background(), key); err != nil {
					t.Errorf("deleting S3 test key %q: %v", key, err)
				}
			}
		})
		oldThumbnail := []byte("old thumbnail")
		oldDigest := sha256.Sum256(oldThumbnail)
		thumbnailID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
			p.Kind, p.RelPath, p.SizeBytes = store.AssetKindThumbnail, oldThumbnailKey, int64(len(oldThumbnail))
		})
		if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = $2 WHERE media_asset_id = $1`, thumbnailID, objectStoreName); err != nil {
			t.Fatalf("placing the thumbnail copy in the S3 store: %v", err)
		}
		if err := sqlcgen.New(pool).UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbnailID, SeekMs: 30000,
		}); err != nil {
			t.Fatalf("seeding current thumbnail seek: %v", err)
		}
		if err := sqlcgen.New(pool).SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID, CmRanges: "{[0,60000)}",
		}); err != nil {
			t.Fatalf("seeding CM ranges: %v", err)
		}
		if err := objects.Put(ctx, oldThumbnailKey, strings.NewReader(string(oldThumbnail)), int64(len(oldThumbnail)), oldDigest, "image/jpeg"); err != nil {
			t.Fatalf("uploading current thumbnail: %v", err)
		}

		mediaDir := t.TempDir()
		if err := mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName); err != nil {
			t.Fatalf("writing local media store marker: %v", err)
		}
		verifiedRemoteInput := false
		worker := &ThumbnailWorker{
			Media: media, Objects: objects, ObjectPublisher: media, MediaDir: mediaDir,
			MediaStoreName: dbtest.StoreName, DerivedStoreName: objectStoreName, ScratchDir: t.TempDir(),
			runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if containsArg(args, "format=duration") {
					t.Fatal("chapter-aware reselection must not probe duration")
				}
				inputIndex := indexOfArg(args, "-i")
				if inputIndex < 0 || inputIndex+1 >= len(args) {
					t.Fatalf("ffmpeg args have no input path: %v", args)
				}
				got, err := os.ReadFile(args[inputIndex+1])
				if err != nil {
					t.Fatalf("reading materialized S3 thumbnail input %q: %v", args[inputIndex+1], err)
				}
				if !bytes.Equal(got, encodedPayload) {
					t.Fatalf("materialized thumbnail input = %q, want remote object bytes %q", got, encodedPayload)
				}
				verifiedRemoteInput = true
				if err := os.WriteFile(args[len(args)-1], tinyJPEG, 0o600); err != nil {
					t.Fatalf("writing generated thumbnail: %v", err)
				}
				return nil, nil
			},
		}
		if err := worker.Work(ctx, jobs.ThumbnailJobArgs{RecordingID: recordingID}); err != nil {
			t.Fatalf("reselecting thumbnail from S3 encoded input: %v", err)
		}
		if !verifiedRemoteInput {
			t.Fatal("thumbnail generation did not read the S3 encoded input")
		}
		row, err := sqlcgen.New(pool).LockActiveThumbnailMediaAsset(ctx, sqlcgen.LockActiveThumbnailMediaAssetParams{
			RecordingID: recordingID, StoreName: objectStoreName,
		})
		if err != nil {
			t.Fatalf("loading replacement thumbnail: %v", err)
		}
		if row.RelPath != newThumbnailKey || row.SeekMs == nil || *row.SeekMs != 89993 {
			t.Fatalf("replacement thumbnail = (%q, %v), want (%q, 89993)", row.RelPath, row.SeekMs, newThumbnailKey)
		}
		object, err := objects.Head(ctx, newThumbnailKey)
		if err != nil || object.Size != int64(len(tinyJPEG)) {
			t.Fatalf("replacement S3 thumbnail = (%+v, %v), want uploaded JPEG", object, err)
		}
	})

	t.Run("DB-less S3 keys are reported and retained", func(t *testing.T) {
		key := prefix + "-unknown.jpg"
		payload := []byte("unknown keys are not automatically deleted")
		digest := sha256.Sum256(payload)
		if err := objects.Put(ctx, key, strings.NewReader(string(payload)), int64(len(payload)), digest, "image/jpeg"); err != nil {
			t.Fatalf("uploading DB-less object: %v", err)
		}
		cleanup := env.deleter(100)
		if err := cleanup.reconcileObjectOrphans(ctx, -time.Second, 0); err != nil {
			t.Fatalf("reconciling unknown S3 key: %v", err)
		}
		if _, err := objects.Head(ctx, key); err != nil {
			t.Fatalf("DB-less S3 key was removed instead of retained: %v", err)
		}
		aged, err := media.ListAgedOrphanFiles(ctx, 0)
		if err != nil {
			t.Fatalf("listing reported DB-less key: %v", err)
		}
		if !containsString(aged, key) {
			t.Fatalf("DB-less key %q is absent from aged orphan report %v", key, aged)
		}
	})

	t.Run("FS derived copy transfers to S3 across slices", func(t *testing.T) {
		// 転送元の FS 置き場の worker が S3 へ直接送る。S3 のパートは 5 MiB 以上なので、帯域の上限と短い slice で
		// 複数の slice に分け、途中の保存から続けて完了することを確かめる。
		root := t.TempDir()
		if err := mediapath.WriteStoreMarker(root, dbtest.StoreName); err != nil {
			t.Fatalf("writing source store marker: %v", err)
		}
		recordingID := dbtest.Recording(t, pool)
		profile := "h264"
		relPath := fmt.Sprintf("encoded/s3-fs-transfer-%d.mp4", time.Now().UnixNano())
		content := bytes.Repeat([]byte("fs-to-s3-transfer-"), 3<<20)
		assetID := seedEncodedOrThumbnailAsset(t, pool, root, recordingID, store.AssetKindEncoded, &profile, relPath, content)
		t.Cleanup(func() {
			if err := objects.Delete(context.Background(), relPath); err != nil {
				t.Errorf("deleting S3 transfer key %q: %v", relPath, err)
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
		if err != nil {
			t.Fatal(err)
		}
		restoreSlice := storageTransferSlice
		storageTransferSlice = time.Second
		t.Cleanup(func() { storageTransferSlice = restoreSlice })
		sourceFS, err := fsstore.New(root)
		if err != nil {
			t.Fatal(err)
		}
		worker := StorageTransferWorker{
			Storage: pgstore.NewMedia(pool, dbtest.StoreName, ""), Config: cfg,
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
		if got := env.copyState(t, relPath); got != "active" {
			t.Fatalf("S3 copy state = %q, want active", got)
		}
		object, err := objects.Head(ctx, relPath)
		if err != nil {
			t.Fatalf("heading transferred S3 object: %v", err)
		}
		if object.Size != int64(len(content)) || object.SHA256 != fmt.Sprintf("%x", sha256.Sum256(content)) {
			t.Fatalf("transferred S3 metadata = (size %d, sha256 %q), want (%d, %x)", object.Size, object.SHA256, len(content), sha256.Sum256(content))
		}

		// 完了した upload の送信状態は、進捗の行が消えるときに掃除待ちへ移る。その行が実 MinIO の Abort で
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
			t.Fatalf("abandoned uploads on %s after cleanup = %d, want 0 (MinIO must answer NoSuchUpload for a completed upload)", objectStoreName, n)
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
