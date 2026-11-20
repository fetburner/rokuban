package storagemaint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mediapublish"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// objectStoreName は S3 の派生物の置き場の名前。FS の原本の置き場（dbtest.StoreName）とは別にする。
const objectStoreName = "s3main"

// objectEnv は S3 の置き場の代わりに fsstore を使う統合テストの環境である。
// 公開と削除の DB の振る舞いを測るので、object store は objstore.Store の契約を満たせば足りる。
// 実際の S3 との適合は s3conformance タグのテストが測る。
type objectEnv struct {
	pool    *pgxpool.Pool
	objects objstore.Store
	media   *pgstore.Media
}

func newObjectEnv(t *testing.T) objectEnv {
	t.Helper()
	return newObjectEnvWith(t, testFilesystemObjectStore(t, t.TempDir()))
}

func testFilesystemObjectStore(t *testing.T, root string) objstore.Store {
	t.Helper()
	objects, err := fsstore.New(root)
	testutil.RequireNoError(t, err, "creating filesystem object store")
	return objects
}

func newObjectEnvWith(t *testing.T, objects objstore.Store) objectEnv {
	t.Helper()
	pool := testutil.SetupDB(t)
	return objectEnv{
		pool: pool, objects: objects,
		media: pgstore.NewMediaForDerivedStore(pool, objectStoreName, dbtest.StoreName, store.NewStoragePolicy(nil, nil, objectStoreName)),
	}
}

func (e objectEnv) deleter(t *testing.T, maxPerPass int) *DeleteReconcileWorker {
	t.Helper()
	derivedDeleteStores, err := store.NewDerivedObjectDeleteStores(e.media, e.media, e.media)
	testutil.RequireNoError(t, err, "creating derived object delete stores")
	return &DeleteReconcileWorker{
		Breakers:                  pgstore.NewBreakers(e.pool),
		MediaStoreName:            dbtest.StoreName,
		DerivedObjects:            e.objects,
		DerivedObjectStoreName:    objectStoreName,
		DerivedObjectDeleteStores: derivedDeleteStores,
		MaxDeletesPerPass:         maxPerPass,
	}
}

// publishThumbnail は thumbnail を S3 の置き場へ、予約 → PUT → 公開の順で公開する。
func (e objectEnv) publishThumbnail(
	t *testing.T, recordingID, originalID int64, generation int64, key string, expected *store.ThumbnailObservation, payload []byte,
) string {
	t.Helper()
	scratch := t.TempDir() + "/thumbnail.jpg"
	writeFile(t, scratch, payload)
	pending := store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
		DerivedGeneration: generation, ReserveNewGeneration: true,
	}
	_, published, err := mediapublish.PublishGeneratedObject(context.Background(), e.objects, e.media, pending, scratch, "image/jpeg", false, nil,
		func(size int64, sum [sha256.Size]byte) (bool, error) {
			return e.media.PublishThumbnail(context.Background(), store.ThumbnailPublishInput{
				RecordingID: recordingID, RelPath: key, SizeBytes: size, SeekMs: 1000, InputAssetID: originalID,
				DerivedGeneration: generation, Expected: expected, SHA256: sum[:], PendingCopy: true,
			}, func() error { return nil })
		})
	if err != nil || !published {
		t.Fatalf("publishing thumbnail generation %d = (%t, %v), want published", generation, published, err)
	}
	return key
}

func writeFile(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func (e objectEnv) copyState(t *testing.T, key string) string {
	t.Helper()
	var state string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2 ORDER BY id DESC LIMIT 1`,
		objectStoreName, key).Scan(&state); err != nil {
		t.Fatalf("reading state of copy %q: %v", key, err)
	}
	return state
}

func (e objectEnv) objectExists(t *testing.T, key string) bool {
	t.Helper()
	_, err := e.objects.Head(context.Background(), key)
	if err != nil && !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("heading object %q: %v", key, err)
	}
	return err == nil
}

// replaceThumbnail は 2 世代の thumbnail を公開し、旧世代と新世代の key を返す。
func (e objectEnv) replaceThumbnail(t *testing.T) (oldKey, newKey string) {
	t.Helper()
	recordingID := dbtest.Recording(t, e.pool)
	originalID := dbtest.MediaAsset(t, e.pool, recordingID)
	oldKey = e.publishThumbnail(t, recordingID, originalID, 0, fmt.Sprintf("thumbnails/%d.jpg", recordingID), nil, []byte("thumbnail generation zero"))
	var assetID int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`, recordingID).Scan(&assetID); err != nil {
		t.Fatalf("loading thumbnail asset: %v", err)
	}
	seek := int64(1000)
	newKey = e.publishThumbnail(t, recordingID, originalID, 1, fmt.Sprintf("thumbnails/%d.g1.jpg", recordingID),
		&store.ThumbnailObservation{ID: assetID, RelPath: oldKey, SeekMs: &seek, DerivedGeneration: 0},
		[]byte("thumbnail generation one"))
	return oldKey, newKey
}

// objectPublishOwnershipPublisher records whether cleanup asks the database who owns a key.
// 旧世代は公開の tx では触らず、削除 reconcile が overlap の後に消す。
// 署名付き URL で旧世代を再生中の視聴者が、公開の直後に 404 にならないためである。
func TestObjectPublishKeepsPreviousGenerationUntilOverlapElapses(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	oldKey, newKey := env.replaceThumbnail(t)

	if got := env.copyState(t, oldKey); got != "active" {
		t.Fatalf("previous generation copy state right after publication = %q, want active", got)
	}
	if got := env.copyState(t, newKey); got != "active" {
		t.Fatalf("new generation copy state = %q, want active", got)
	}
	if !env.objectExists(t, oldKey) {
		t.Fatal("previous generation object was removed by publication, want it kept for the overlap")
	}

	worker := env.deleter(t, 100)
	worker.TransferOverlap = 48 * time.Hour
	testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile inside the overlap")
	if !env.objectExists(t, oldKey) || env.copyState(t, oldKey) != "active" {
		t.Fatal("previous generation was deleted inside the overlap window")
	}

	testutil.MustExec(t, env.pool, "aging the new generation",
		`UPDATE media_asset_copies SET created_at = now() - interval '49 hours' WHERE store = $1 AND rel_path = $2`,
		objectStoreName, newKey)
	testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile after the overlap")
	if env.objectExists(t, oldKey) || env.copyState(t, oldKey) != "deleted" {
		t.Fatalf("previous generation after the overlap: object exists %t, copy state %q; want removed and deleted",
			env.objectExists(t, oldKey), env.copyState(t, oldKey))
	}
	if !env.objectExists(t, newKey) || env.copyState(t, newKey) != "active" {
		t.Fatal("the current generation was removed together with the previous one")
	}
}

// 削除ブレーカーの範囲は派生物の置き場の名前である。S3 の件数を FS の置き場のブレーカーへ足さず、
// FS の置き場のブレーカーが発動していても S3 の削除を止めない。
func TestObjectDeleteBreakerIsScopedToTheDerivedStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scopes := func(env objectEnv) []string {
		rows, err := env.pool.Query(ctx, `SELECT scope FROM circuit_breakers WHERE name = $1 ORDER BY scope`, breaker.DeleteReconcile)
		testutil.RequireNoError(t, err, "listing breakers")
		defer rows.Close()
		var out []string
		for rows.Next() {
			var scope string
			testutil.RequireNoError(t, rows.Scan(&scope), "scanning breaker scope")
			out = append(out, scope)
		}
		return out
	}
	ageAll := func(env objectEnv) {
		testutil.MustExec(t, env.pool, "aging copies", `UPDATE media_asset_copies SET created_at = now() - interval '72 hours' WHERE store = $1`, objectStoreName)
	}

	t.Run("exceeding the limit trips only the derived store scope", func(t *testing.T) {
		env := newObjectEnv(t)
		var oldKeys []string
		for range 3 {
			oldKey, _ := env.replaceThumbnail(t)
			oldKeys = append(oldKeys, oldKey)
		}
		ageAll(env)
		worker := env.deleter(t, 2)
		worker.TransferOverlap = 48 * time.Hour
		testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile over the limit")
		if got := scopes(env); len(got) != 1 || got[0] != objectStoreName {
			t.Fatalf("tripped breaker scopes = %v, want only [%s]", got, objectStoreName)
		}
		for _, key := range oldKeys {
			if !env.objectExists(t, key) {
				t.Fatalf("object %q was deleted although the breaker tripped", key)
			}
		}
	})

	t.Run("a tripped filesystem scope does not stop S3 deletes", func(t *testing.T) {
		env := newObjectEnv(t)
		oldKey, _ := env.replaceThumbnail(t)
		ageAll(env)
		testutil.RequireNoError(t, breaker.Trip(ctx, pgstore.NewBreakers(env.pool), dbtest.StoreName, breaker.DeleteReconcile, 1, breaker.Sample{Total: 2}), "tripping the filesystem scope")
		worker := env.deleter(t, 100)
		worker.TransferOverlap = 48 * time.Hour
		testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile")
		if env.objectExists(t, oldKey) {
			t.Fatal("a tripped breaker of the filesystem store stopped the S3 delete")
		}
	})

	t.Run("a tripped derived store scope stops S3 deletes", func(t *testing.T) {
		env := newObjectEnv(t)
		oldKey, _ := env.replaceThumbnail(t)
		ageAll(env)
		testutil.RequireNoError(t, breaker.Trip(ctx, pgstore.NewBreakers(env.pool), objectStoreName, breaker.DeleteReconcile, 1, breaker.Sample{Total: 2}), "tripping the derived store scope")
		worker := env.deleter(t, 100)
		worker.TransferOverlap = 48 * time.Hour
		testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile")
		if !env.objectExists(t, oldKey) {
			t.Fatal("the derived store breaker was ignored and the object was deleted")
		}
	})
}

// 公開されないまま猶予を過ぎた pending の key の回収は、新しい削除の判断としてブレーカーの件数に数える。
func TestObjectPendingReclaimCountsTowardTheBreaker(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	var keys []string
	for _, key := range []string{"thumbnails/first.jpg", "thumbnails/second.jpg"} {
		recordingID := dbtest.Recording(t, env.pool)
		testutil.RequireNoError(t, env.media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
			RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
		}), "reserving pending copy")
		payload := []byte("uploaded before the worker stopped " + key)
		testutil.RequireNoError(t, env.objects.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "image/jpeg"), "uploading object")
		keys = append(keys, key)
	}
	testutil.MustExec(t, env.pool, "aging pending copies", `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1`, objectStoreName)

	worker := env.deleter(t, 1)
	testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile")
	for _, key := range keys {
		if !env.objectExists(t, key) {
			t.Fatalf("pending object %q was reclaimed before the breaker was consulted", key)
		}
	}

	testutil.MustExec(t, env.pool, "resuming the breaker", `DELETE FROM circuit_breakers`)
	worker = env.deleter(t, 100)
	testutil.RequireNoError(t, worker.workObjectStore(ctx, worker.settings()), "delete reconcile after resume")
	for _, key := range keys {
		if env.objectExists(t, key) || env.copyState(t, key) != "deleted" {
			t.Fatalf("aged pending %q: object exists %t, state %q; want removed and deleted", key, env.objectExists(t, key), env.copyState(t, key))
		}
	}
}

// 共有 FS の派生物も Store.Put 後に公開が失敗した pending key を、通常の FS reconcile が
// ブレーカーに数えたうえで Store.Delete 経由で回収する。
func TestFilesystemDerivedPendingReclaimAfterPutFailure(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName), "writing media store marker")
	objects := testFSObjectStore(mediaDir)
	media := pgstore.NewMediaInStore(pool, dbtest.StoreName, store.NewStoragePolicy(nil, nil, dbtest.StoreName))
	keys := make([]string, 0, 2)
	for _, key := range []string{"thumbnails/first.jpg", "thumbnails/second.jpg"} {
		recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "finished" })
		scratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
		writeFile(t, scratch, []byte("uploaded before publication failed"))
		_, published, err := mediapublish.PublishGeneratedObject(ctx, objects, media,
			store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key},
			scratch, "image/jpeg", false, nil,
			func(int64, [sha256.Size]byte) (bool, error) {
				return false, errors.New("injected DB publication failure")
			},
		)
		if err == nil || published {
			t.Fatalf("publication result = (%t, %v), want failed after Put", published, err)
		}
		if _, err := objects.Head(ctx, key); err != nil {
			t.Fatalf("Head failed object %q: %v", key, err)
		}
		keys = append(keys, key)
	}
	for _, key := range keys {
		if _, err := pool.Exec(ctx,
			`UPDATE media_asset_copies SET created_at = now() - interval '48 hours' WHERE store = $1 AND rel_path = $2`,
			dbtest.StoreName, key); err != nil {
			t.Fatalf("aging pending copy %q: %v", key, err)
		}
	}
	originalRelPath := "original/pending-transfer.ts"
	originalPath := filepath.Join(mediaDir, filepath.FromSlash(originalRelPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(originalPath), 0o755), "creating original directory")
	testutil.RequireNoError(t, os.WriteFile(originalPath, []byte("original remains file-backed"), 0o644), "writing pending original")
	originalRecordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "finished" })
	originalAssetID := dbtest.MediaAsset(t, pool, originalRecordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath, p.SizeBytes = originalRelPath, int64(len("original remains file-backed"))
	})
	testutil.MustExec(t, pool, "aging pending original copy",
		`UPDATE media_asset_copies SET state = 'pending', created_at = now() - interval '48 hours' WHERE media_asset_id = $1 AND store = $2`,
		originalAssetID, dbtest.StoreName)

	worker := &DeleteReconcileWorker{
		MediaDelete: media, MediaPendingReclaim: media,
		Storage: media, RecordingPurge: pgstore.NewRecordings(pool, dbtest.StoreName),
		Breakers: pgstore.NewBreakers(pool), MediaDir: mediaDir, StorageObjects: testStorageObjects(t, dbtest.StoreName, mediaDir), MediaStoreName: dbtest.StoreName,
		DerivedObjects: objects, DerivedObjectStoreName: dbtest.StoreName,
		TrashRetention: 24 * time.Hour, OrphanMTimeGrace: 24 * time.Hour, OrphanAge: 24 * time.Hour,
		MissingAssetAge: 24 * time.Hour, MaxDeletesPerPass: 1,
	}
	testutil.RequireNoError(t, worker.workFilesystem(ctx, worker.settings()), "delete reconcile with breaker limit")
	var breakerCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM circuit_breakers WHERE name = $1 AND scope = $2`, breaker.DeleteReconcile, dbtest.StoreName,
	).Scan(&breakerCount); err != nil {
		t.Fatalf("checking filesystem delete breaker: %v", err)
	}
	if breakerCount != 1 {
		t.Fatalf("filesystem delete breaker rows = %d, want 1 for two pending copies over the limit", breakerCount)
	}
	for _, key := range keys {
		if _, err := objects.Head(ctx, key); err != nil {
			t.Fatalf("pending object %q was deleted while breaker was latched: %v", key, err)
		}
		var state string
		if err := pool.QueryRow(ctx,
			`SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, dbtest.StoreName, key,
		).Scan(&state); err != nil {
			t.Fatalf("reading pending copy state %q: %v", key, err)
		}
		if state != "pending" {
			t.Fatalf("pending copy %q state = %q, want pending while breaker is latched", key, state)
		}
	}
	testutil.MustExec(t, pool, "resuming filesystem delete breaker", `DELETE FROM circuit_breakers WHERE name = $1 AND scope = $2`, breaker.DeleteReconcile, dbtest.StoreName)
	worker.MaxDeletesPerPass = 100
	testutil.RequireNoError(t, worker.workFilesystem(ctx, worker.settings()), "delete reconcile after resuming breaker")
	for _, key := range keys {
		if _, err := objects.Head(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
			t.Errorf("Head after pending reclaim for %q = %v, want ErrNotFound", key, err)
		}
		var state string
		if err := pool.QueryRow(ctx,
			`SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, dbtest.StoreName, key,
		).Scan(&state); err != nil {
			t.Fatalf("reading reclaimed copy state %q: %v", key, err)
		}
		if state != "deleted" {
			t.Errorf("reclaimed copy %q state = %q, want deleted", key, state)
		}
	}
	if _, err := os.Stat(originalPath); err != nil {
		t.Errorf("pending original was changed by derived reclaim: %v", err)
	}
	var originalState string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, originalAssetID, dbtest.StoreName,
	).Scan(&originalState); err != nil {
		t.Fatalf("reading original copy state: %v", err)
	}
	if originalState != "pending" {
		t.Errorf("original copy state = %q, want pending", originalState)
	}
}

func TestFilesystemDerivedPendingReclaimKeepsMainUntilSubtitleRemovalSucceeds(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName), "writing media store marker")
	objects := testFilesystemObjectStore(t, mediaDir)
	media := pgstore.NewMediaInStore(pool, dbtest.StoreName, store.NewStoragePolicy(nil, nil, dbtest.StoreName))
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	relPath := "pending/sidecar-retry.mp4"
	assetID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, relPath, []byte("pending encoded object"))
	subtitleRelPath, err := mediapath.SubtitleSibling(relPath)
	testutil.RequireNoError(t, err, "mediapath.SubtitleSibling")
	subtitlePath := filepath.Join(mediaDir, filepath.FromSlash(subtitleRelPath))
	if err := os.Remove(subtitlePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removing seeded subtitle: %v", err)
	}
	testutil.RequireNoError(t, os.MkdirAll(subtitlePath, 0o755), "creating blocked subtitle directory")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(subtitlePath, "keep.txt"), []byte("x"), 0o644), "making subtitle deletion fail")
	copyID := transferTestCopyID(t, pool, assetID, dbtest.StoreName)
	testutil.MustExec(t, pool, "aging pending encoded copy", `UPDATE media_asset_copies SET state = 'pending', created_at = now() - interval '48 hours' WHERE id = $1`, copyID)
	worker := &DeleteReconcileWorker{
		MediaDelete: media, MediaPendingReclaim: media, Storage: media,
		RecordingPurge: pgstore.NewRecordings(pool, dbtest.StoreName), Breakers: pgstore.NewBreakers(pool),
		MediaDir: mediaDir, StorageObjects: testStorageObjects(t, dbtest.StoreName, mediaDir), MediaStoreName: dbtest.StoreName,
		DerivedObjects: objects, DerivedObjectStoreName: dbtest.StoreName, OrphanAge: time.Hour,
	}
	testutil.RequireNoError(t, worker.Work(ctx, jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work with blocked subtitle")
	if _, err := objects.Head(ctx, relPath); err != nil {
		t.Fatalf("main object was removed before subtitle cleanup succeeded: %v", err)
	}
	var state string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, copyID).Scan(&state), "reading copy state after blocked subtitle cleanup")
	if state != "pending" {
		t.Fatalf("copy state after blocked subtitle cleanup = %q, want pending", state)
	}

	testutil.RequireNoError(t, os.RemoveAll(subtitlePath), "removing blocked subtitle directory")
	testutil.RequireNoError(t, os.WriteFile(subtitlePath, []byte("WEBVTT\n\ncaption\n"), 0o644), "restoring subtitle object")
	testutil.RequireNoError(t, worker.Work(ctx, jobs.DeleteReconcileArgs{}), "retrying DeleteReconcileWorker.Work")
	if _, err := objects.Head(ctx, relPath); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("main object after successful retry = %v, want ErrNotFound", err)
	}
	if _, err := objects.Head(ctx, subtitleRelPath); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("subtitle object after successful retry = %v, want ErrNotFound", err)
	}
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, copyID).Scan(&state), "reading copy state after successful retry")
	if state != "deleted" {
		t.Errorf("copy state after successful retry = %q, want deleted", state)
	}
}

// 回収は pending の行をロックしたまま key を消す。公開の tx が先に active にした行は消さない。
func TestObjectPendingReclaimSkipsCopyPublishedFirst(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	key := env.publishThumbnail(t, recordingID, originalID, 0, "thumbnails/poster.jpg", nil, []byte("published"))
	testutil.MustExec(t, env.pool, "aging the copy", `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1`, objectStoreName)
	var copyID int64
	testutil.RequireNoError(t, env.pool.QueryRow(ctx, `SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, objectStoreName, key).Scan(&copyID), "loading copy")
	called := false
	reclaimed, err := env.media.ReclaimPendingMediaAssetCopy(ctx, copyID, time.Now(), func() error { called = true; return nil })
	if err != nil || reclaimed || called {
		t.Fatalf("reclaiming an active copy = (%t, %v, deleter called %t), want (false, nil, false)", reclaimed, err, called)
	}
	if !env.objectExists(t, key) || env.copyState(t, key) != "active" {
		t.Fatal("a copy published before the reclaim lost its object")
	}
}

// 回収された pending は公開できない。公開の tx は ErrStalePublishPlan を返し、encode は計画を立て直す。
func TestObjectPublishFailsWhenPendingCopyWasReclaimed(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	key := "thumbnails/poster.jpg"
	testutil.RequireNoError(t, env.media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
	}), "reserving pending copy")
	var copyID int64
	testutil.RequireNoError(t, env.pool.QueryRow(ctx, `SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, objectStoreName, key).Scan(&copyID), "loading copy")
	testutil.MustExec(t, env.pool, "aging the copy", `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE id = $1`, copyID)
	reclaimed, err := env.media.ReclaimPendingMediaAssetCopy(ctx, copyID, time.Now(), func() error { return nil })
	if err != nil || !reclaimed {
		t.Fatalf("reclaiming pending copy = (%t, %v), want (true, nil)", reclaimed, err)
	}
	published, err := env.media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
		RecordingID: recordingID, RelPath: key, SizeBytes: 1, SeekMs: 1000, InputAssetID: originalID,
		PendingCopy: true,
	}, func() error { return nil })
	if published || !errors.Is(err, store.ErrStalePublishPlan) || !errors.Is(err, store.ErrPendingMediaAssetCopyMissing) {
		t.Fatalf("publishing a reclaimed pending copy = (%t, %v), want stale-plan and missing-copy errors", published, err)
	}
	if got := env.copyState(t, key); got != "deleted" {
		t.Fatalf("reclaimed copy state = %q, want deleted", got)
	}
}
