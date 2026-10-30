package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
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
	objects, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("creating object store: %v", err)
	}
	return newObjectEnvWith(t, objects)
}

func newObjectEnvWith(t *testing.T, objects objstore.Store) objectEnv {
	t.Helper()
	pool := setupTestPool(t)
	return objectEnv{
		pool: pool, objects: objects,
		media: pgstore.NewMediaWithStores(pool, objectStoreName, dbtest.StoreName, objectStoreName),
	}
}

func (e objectEnv) deleter(maxPerPass int) *DeleteReconcileWorker {
	return &DeleteReconcileWorker{
		Breakers:               pgstore.NewBreakers(e.pool),
		MediaStoreName:         dbtest.StoreName,
		DerivedObjects:         e.objects,
		DerivedObjectStoreName: objectStoreName,
		DerivedMediaDelete:     e.media,
		DerivedPendingReclaim:  e.media,
		DerivedStorage:         e.media,
		MaxDeletesPerPass:      maxPerPass,
	}
}

// publishThumbnail は thumbnail を S3 の置き場へ、予約 → PUT → 公開の順で公開する。
func (e objectEnv) publishThumbnail(
	t *testing.T, recordingID, originalID int64, generation int64, expected *store.ThumbnailObservation, payload []byte,
) string {
	t.Helper()
	key := thumbnailRelPath(recordingID, generation)
	scratch := t.TempDir() + "/thumbnail.jpg"
	writeFile(t, scratch, payload)
	pending := store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key, DerivedGeneration: generation,
	}
	_, published, err := publishGeneratedObject(context.Background(), e.objects, e.media, pending, scratch, "image/jpeg", false, nil,
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

// readObject は object の全体を読む。fsstore は Open、S3 は Get で読む。
func (e objectEnv) readObject(t *testing.T, key string) []byte {
	t.Helper()
	var reader io.ReadCloser
	switch objects := e.objects.(type) {
	case objstore.Reader:
		body, err := objects.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("getting object %q: %v", key, err)
		}
		reader = body
	case objstore.Local:
		body, _, err := objects.Open(context.Background(), key)
		if err != nil {
			t.Fatalf("opening object %q: %v", key, err)
		}
		reader = body
	default:
		t.Fatalf("object store %T cannot be read", e.objects)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading object %q: %v", key, err)
	}
	return body
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
	oldKey = e.publishThumbnail(t, recordingID, originalID, 0, nil, []byte("thumbnail generation zero"))
	var assetID int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`, recordingID).Scan(&assetID); err != nil {
		t.Fatalf("loading thumbnail asset: %v", err)
	}
	seek := int64(1000)
	newKey = e.publishThumbnail(t, recordingID, originalID, 1,
		&store.ThumbnailObservation{ID: assetID, RelPath: oldKey, SeekMs: &seek, DerivedGeneration: 0},
		[]byte("thumbnail generation one"))
	return oldKey, newKey
}

// 旧世代は公開の tx では触らず、削除 reconcile が overlap の後に消す。
// 署名付き URL で旧世代を再生中の視聴者が、公開の直後に 404 にならないためである。
func TestObjectPublishKeepsPreviousGenerationUntilOverlapElapses(t *testing.T) {
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

	worker := env.deleter(100)
	worker.TransferOverlap = 48 * time.Hour
	if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
		t.Fatalf("delete reconcile inside the overlap: %v", err)
	}
	if !env.objectExists(t, oldKey) || env.copyState(t, oldKey) != "active" {
		t.Fatal("previous generation was deleted inside the overlap window")
	}

	if _, err := env.pool.Exec(ctx,
		`UPDATE media_asset_copies SET created_at = now() - interval '49 hours' WHERE store = $1 AND rel_path = $2`,
		objectStoreName, newKey); err != nil {
		t.Fatalf("aging the new generation: %v", err)
	}
	if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
		t.Fatalf("delete reconcile after the overlap: %v", err)
	}
	if env.objectExists(t, oldKey) || env.copyState(t, oldKey) != "deleted" {
		t.Fatalf("previous generation after the overlap: object exists %t, copy state %q; want removed and deleted",
			env.objectExists(t, oldKey), env.copyState(t, oldKey))
	}
	if !env.objectExists(t, newKey) || env.copyState(t, newKey) != "active" {
		t.Fatal("the current generation was removed together with the previous one")
	}
}

// SHA-256 は作り手が commit の tx で論理行へ書く。S3 の metadata にだけ載せると、
// 論理行は NULL のまま残り、転送の照合が使えない。
func TestObjectPublishWritesSHA256InTheCommitTransaction(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	profile := "h264"
	seedEncodePolicy(t, env.pool, recordingID, []string{profile})
	if _, err := sqlcgen.New(env.pool).CreateRecordingEncodeAttemptRunning(ctx, sqlcgen.CreateRecordingEncodeAttemptRunningParams{
		RecordingID: recordingID, Profile: profile,
	}); err != nil {
		t.Fatalf("seeding running encode attempt: %v", err)
	}

	thumbnail := []byte("thumbnail bytes")
	env.publishThumbnail(t, recordingID, originalID, 0, nil, thumbnail)

	tiles := []byte("tile sheet bytes")
	tilesKey := seekTilesRelPath(recordingID, 0)
	tilesPath := t.TempDir() + "/tiles.jpg"
	writeFile(t, tilesPath, tiles)
	_, published, err := publishGeneratedObject(ctx, env.objects, env.media,
		store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindSeekTiles, RelPath: tilesKey}, tilesPath, "image/jpeg", false, nil,
		func(size int64, sum [sha256.Size]byte) (bool, error) {
			return env.media.PublishSeekTiles(ctx, store.SeekTilesPublishInput{
				RecordingID: recordingID, RelPath: tilesKey, SizeBytes: size, SHA256: sum[:], PendingCopy: true,
			}, func() error { return nil })
		})
	if err != nil || !published {
		t.Fatalf("publishing seek tiles = (%t, %v), want published", published, err)
	}

	encoded := []byte("encoded bytes")
	encodedKey := "encoded/sha256-test.mp4"
	encodedPath := t.TempDir() + "/encoded.mp4"
	writeFile(t, encodedPath, encoded)
	_, published, err = publishGeneratedObject(ctx, env.objects, env.media,
		store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindEncoded, Profile: &profile, RelPath: encodedKey, ReserveNewGeneration: true},
		encodedPath, "video/mp4", false, nil,
		func(size int64, sum [sha256.Size]byte) (bool, error) {
			return env.media.PublishEncoded(ctx, store.EncodedPublishInput{
				RecordingID: recordingID, Profile: profile, Attempt: 1, RelPath: encodedKey, SizeBytes: size,
				SHA256: sum[:], PendingCopy: true,
			}, func() error { return nil })
		})
	if err != nil || !published {
		t.Fatalf("publishing encoded = (%t, %v), want published", published, err)
	}

	for kind, payload := range map[string][]byte{"thumbnail": thumbnail, "seek_tiles": tiles, "encoded": encoded} {
		var got []byte
		if err := env.pool.QueryRow(ctx,
			`SELECT sha256 FROM media_assets WHERE recording_id = $1 AND kind = $2`, recordingID, kind).Scan(&got); err != nil {
			t.Fatalf("reading %s sha256: %v", kind, err)
		}
		want := sha256.Sum256(payload)
		if !bytes.Equal(got, want[:]) {
			t.Errorf("%s media_assets.sha256 = %x, want %x", kind, got, want)
		}
	}
}

// 削除ブレーカーの範囲は派生物の置き場の名前である。S3 の件数を FS の置き場のブレーカーへ足さず、
// FS の置き場のブレーカーが発動していても S3 の削除を止めない。
func TestObjectDeleteBreakerIsScopedToTheDerivedStore(t *testing.T) {
	ctx := context.Background()
	scopes := func(env objectEnv) []string {
		rows, err := env.pool.Query(ctx, `SELECT scope FROM circuit_breakers WHERE name = $1 ORDER BY scope`, breaker.DeleteReconcile)
		if err != nil {
			t.Fatalf("listing breakers: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var scope string
			if err := rows.Scan(&scope); err != nil {
				t.Fatalf("scanning breaker scope: %v", err)
			}
			out = append(out, scope)
		}
		return out
	}
	ageAll := func(env objectEnv) {
		if _, err := env.pool.Exec(ctx, `UPDATE media_asset_copies SET created_at = now() - interval '72 hours' WHERE store = $1`, objectStoreName); err != nil {
			t.Fatalf("aging copies: %v", err)
		}
	}

	t.Run("exceeding the limit trips only the derived store scope", func(t *testing.T) {
		env := newObjectEnv(t)
		var oldKeys []string
		for range 3 {
			oldKey, _ := env.replaceThumbnail(t)
			oldKeys = append(oldKeys, oldKey)
		}
		ageAll(env)
		worker := env.deleter(2)
		worker.TransferOverlap = 48 * time.Hour
		if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
			t.Fatalf("delete reconcile over the limit: %v", err)
		}
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
		if err := breaker.Trip(ctx, pgstore.NewBreakers(env.pool), dbtest.StoreName, breaker.DeleteReconcile, 1, breaker.Sample{Total: 2}); err != nil {
			t.Fatalf("tripping the filesystem scope: %v", err)
		}
		worker := env.deleter(100)
		worker.TransferOverlap = 48 * time.Hour
		if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
			t.Fatalf("delete reconcile: %v", err)
		}
		if env.objectExists(t, oldKey) {
			t.Fatal("a tripped breaker of the filesystem store stopped the S3 delete")
		}
	})

	t.Run("a tripped derived store scope stops S3 deletes", func(t *testing.T) {
		env := newObjectEnv(t)
		oldKey, _ := env.replaceThumbnail(t)
		ageAll(env)
		if err := breaker.Trip(ctx, pgstore.NewBreakers(env.pool), objectStoreName, breaker.DeleteReconcile, 1, breaker.Sample{Total: 2}); err != nil {
			t.Fatalf("tripping the derived store scope: %v", err)
		}
		worker := env.deleter(100)
		worker.TransferOverlap = 48 * time.Hour
		if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
			t.Fatalf("delete reconcile: %v", err)
		}
		if !env.objectExists(t, oldKey) {
			t.Fatal("the derived store breaker was ignored and the object was deleted")
		}
	})
}

// 公開されないまま猶予を過ぎた pending の key の回収は、新しい削除の判断としてブレーカーの件数に数える。
func TestObjectPendingReclaimCountsTowardTheBreaker(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	var keys []string
	for range 2 {
		recordingID := dbtest.Recording(t, env.pool)
		key := thumbnailRelPath(recordingID, 0)
		if err := env.media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
			RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
		}); err != nil {
			t.Fatalf("reserving pending copy: %v", err)
		}
		payload := []byte("uploaded before the worker stopped " + key)
		if err := env.objects.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "image/jpeg"); err != nil {
			t.Fatalf("uploading object: %v", err)
		}
		keys = append(keys, key)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1`, objectStoreName); err != nil {
		t.Fatalf("aging pending copies: %v", err)
	}

	worker := env.deleter(1)
	if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}
	for _, key := range keys {
		if !env.objectExists(t, key) {
			t.Fatalf("pending object %q was reclaimed before the breaker was consulted", key)
		}
	}

	if _, err := env.pool.Exec(ctx, `DELETE FROM circuit_breakers`); err != nil {
		t.Fatalf("resuming the breaker: %v", err)
	}
	worker = env.deleter(100)
	if err := worker.workObjectStore(ctx, worker.settings()); err != nil {
		t.Fatalf("delete reconcile after resume: %v", err)
	}
	for _, key := range keys {
		if env.objectExists(t, key) || env.copyState(t, key) != "deleted" {
			t.Fatalf("aged pending %q: object exists %t, state %q; want removed and deleted", key, env.objectExists(t, key), env.copyState(t, key))
		}
	}
}

// 共有 FS の派生物も Store.Put 後に公開が失敗した pending key を、通常の FS reconcile が
// ブレーカーに数えたうえで Store.Delete 経由で回収する。
func TestFilesystemDerivedPendingReclaimAfterPutFailure(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName); err != nil {
		t.Fatalf("writing media store marker: %v", err)
	}
	objects := testFSObjectStore(mediaDir)
	media := pgstore.NewMediaWithStores(pool, dbtest.StoreName, dbtest.StoreName, dbtest.StoreName)
	keys := make([]string, 0, 2)
	for range 2 {
		recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "finished" })
		key := thumbnailRelPath(recordingID, 0)
		scratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
		writeFile(t, scratch, []byte("uploaded before publication failed"))
		_, published, err := publishGeneratedObject(ctx, objects, media,
			store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key},
			scratch, "image/jpeg", false, nil,
			func(int64, [sha256.Size]byte) (bool, error) { return false, errors.New("injected DB publication failure") },
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
	if err := os.MkdirAll(filepath.Dir(originalPath), 0o755); err != nil {
		t.Fatalf("creating original directory: %v", err)
	}
	if err := os.WriteFile(originalPath, []byte("original remains file-backed"), 0o644); err != nil {
		t.Fatalf("writing pending original: %v", err)
	}
	originalRecordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "finished" })
	originalAssetID := dbtest.MediaAsset(t, pool, originalRecordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath, p.SizeBytes = originalRelPath, int64(len("original remains file-backed"))
	})
	if _, err := pool.Exec(ctx,
		`UPDATE media_asset_copies SET state = 'pending', created_at = now() - interval '48 hours' WHERE media_asset_id = $1 AND store = $2`,
		originalAssetID, dbtest.StoreName); err != nil {
		t.Fatalf("aging pending original copy: %v", err)
	}

	worker := &DeleteReconcileWorker{
		MediaDelete: media, MediaPendingReclaim: media,
		Storage: media, RecordingPurge: pgstore.NewRecordings(pool, dbtest.StoreName),
		Breakers: pgstore.NewBreakers(pool), MediaDir: mediaDir, MediaStoreName: dbtest.StoreName,
		DerivedObjects: objects, DerivedObjectStoreName: dbtest.StoreName,
		TrashRetention: 24 * time.Hour, OrphanMTimeGrace: 24 * time.Hour, OrphanAge: 24 * time.Hour,
		MissingAssetAge: 24 * time.Hour, MaxDeletesPerPass: 1,
	}
	if err := worker.workFilesystem(ctx, worker.settings()); err != nil {
		t.Fatalf("delete reconcile with breaker limit: %v", err)
	}
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
	if _, err := pool.Exec(ctx, `DELETE FROM circuit_breakers WHERE name = $1 AND scope = $2`, breaker.DeleteReconcile, dbtest.StoreName); err != nil {
		t.Fatalf("resuming filesystem delete breaker: %v", err)
	}
	worker.MaxDeletesPerPass = 100
	if err := worker.workFilesystem(ctx, worker.settings()); err != nil {
		t.Fatalf("delete reconcile after resuming breaker: %v", err)
	}
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

// 回収は pending の行をロックしたまま key を消す。公開の tx が先に active にした行は消さない。
func TestObjectPendingReclaimSkipsCopyPublishedFirst(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	key := env.publishThumbnail(t, recordingID, originalID, 0, nil, []byte("published"))
	if _, err := env.pool.Exec(ctx, `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1`, objectStoreName); err != nil {
		t.Fatalf("aging the copy: %v", err)
	}
	var copyID int64
	if err := env.pool.QueryRow(ctx, `SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, objectStoreName, key).Scan(&copyID); err != nil {
		t.Fatalf("loading copy: %v", err)
	}
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
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	key := thumbnailRelPath(recordingID, 0)
	if err := env.media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
	}); err != nil {
		t.Fatalf("reserving pending copy: %v", err)
	}
	var copyID int64
	if err := env.pool.QueryRow(ctx, `SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, objectStoreName, key).Scan(&copyID); err != nil {
		t.Fatalf("loading copy: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE id = $1`, copyID); err != nil {
		t.Fatalf("aging the copy: %v", err)
	}
	reclaimed, err := env.media.ReclaimPendingMediaAssetCopy(ctx, copyID, time.Now(), func() error { return nil })
	if err != nil || !reclaimed {
		t.Fatalf("reclaiming pending copy = (%t, %v), want (true, nil)", reclaimed, err)
	}
	published, err := env.media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
		RecordingID: recordingID, RelPath: key, SizeBytes: 1, SeekMs: 1000, InputAssetID: originalID,
		PendingCopy: true,
	}, func() error { return nil })
	if published || !errors.Is(err, store.ErrStalePublishPlan) {
		t.Fatalf("publishing a reclaimed pending copy = (%t, %v), want (false, ErrStalePublishPlan)", published, err)
	}
	if got := env.copyState(t, key); got != "deleted" {
		t.Fatalf("reclaimed copy state = %q, want deleted", got)
	}
}

// encode が ffmpeg の前に予約した世代は、公開前に止まっても再利用しない。
func TestObjectEncodeReservationNeverReusesAnInterruptedGeneration(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	profile := "h264"
	reserve := func(generation int64) error {
		return env.media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
			RecordingID: recordingID, Kind: store.AssetKindEncoded, Profile: &profile,
			RelPath: fmt.Sprintf("encoded/reuse.u%d.mp4", generation), DerivedGeneration: generation, ReserveNewGeneration: true,
		})
	}
	if err := reserve(1); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if err := reserve(1); !errors.Is(err, store.ErrGenerationAdvanced) {
		t.Fatalf("reserving the generation of an interrupted attempt = %v, want ErrGenerationAdvanced", err)
	}
	if err := reserve(2); err != nil {
		t.Fatalf("reserving the next generation: %v", err)
	}
	state, err := env.media.GetEncodedPublishState(ctx, recordingID, profile, false, false)
	if err != nil {
		t.Fatalf("loading encode publish state: %v", err)
	}
	if state.MaxCopyGeneration == nil || *state.MaxCopyGeneration != 2 {
		t.Fatalf("MaxCopyGeneration = %v, want 2 so that the next plan skips used generations", state.MaxCopyGeneration)
	}
}

// 世代は挿入後に変えない。置き換えは新しい世代の行を足し、旧世代の行を書き換えない。
func TestObjectReplacementAddsRowAndNeverRewritesGeneration(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	oldKey, newKey := env.replaceThumbnail(t)
	rows, err := env.pool.Query(ctx, `SELECT rel_path, generation FROM media_asset_copies WHERE store = $1 ORDER BY id`, objectStoreName)
	if err != nil {
		t.Fatalf("listing copies: %v", err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var key string
		var generation int64
		if err := rows.Scan(&key, &generation); err != nil {
			t.Fatalf("scanning copy: %v", err)
		}
		got[key] = generation
	}
	if len(got) != 2 || got[oldKey] != 0 || got[newKey] != 1 {
		t.Fatalf("copy generations = %v, want {%s: 0, %s: 1}", got, oldKey, newKey)
	}
}

// 公開前に止まった encode の再試行は、新しい世代の key を使う。2 回目の出力が 1 回目と違っても、
// 1 回目の key は上書きされず、公開されるのは 2 回目の key だけである。
func TestObjectEncodeRetryAfterInterruptedPublishUsesNewGeneration(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName); err != nil {
		t.Fatalf("writing local media store marker: %v", err)
	}
	sourceRel := fmt.Sprintf("s3-retry-%d/source.m2ts", time.Now().UnixNano())
	prepareEncodeInput(t, env.pool, recordingID, mediaDir, sourceRel, []string{"h264"}, []byte("original source"))
	initialKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 0, false)
	if err != nil {
		t.Fatalf("building initial encoded key: %v", err)
	}
	retryKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 1, false)
	if err != nil {
		t.Fatalf("building retry encoded key: %v", err)
	}

	worker := newH264Worker(env.pool, mediaDir, t.TempDir(), installCountingFFmpeg(t))
	worker.MediaStoreName = dbtest.StoreName
	worker.Objects = env.objects
	worker.ObjectPublisher = env.media
	worker.Media = env.media
	worker.Attempts = env.media

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
		t.Fatal("first encode succeeded, want a failure after the PUT")
	}
	if !env.objectExists(t, initialKey) || env.copyState(t, initialKey) != "pending" {
		t.Fatalf("after the interrupted attempt: object exists %t, copy state %q; want the uploaded key still pending",
			env.objectExists(t, initialKey), env.copyState(t, initialKey))
	}
	if err := worker.Work(ctx, args); err != nil {
		t.Fatalf("retrying encode: %v", err)
	}
	if env.copyState(t, retryKey) != "active" {
		t.Fatalf("retry copy state = %q, want active at %s", env.copyState(t, retryKey), retryKey)
	}
	if env.copyState(t, initialKey) != "pending" {
		t.Fatalf("interrupted copy state = %q, want it left pending for the reclaim", env.copyState(t, initialKey))
	}
	var generation int64
	var sum []byte
	if err := env.pool.QueryRow(ctx,
		`SELECT COALESCE(derived_generation, 0), sha256 FROM media_assets WHERE recording_id = $1 AND kind = 'encoded'`,
		recordingID).Scan(&generation, &sum); err != nil {
		t.Fatalf("loading encoded asset: %v", err)
	}
	if generation != 1 {
		t.Fatalf("encoded derived_generation = %d, want 1", generation)
	}
	body := env.readObject(t, retryKey)
	want := sha256.Sum256(body)
	if !bytes.Equal(sum, want[:]) {
		t.Fatalf("media_assets.sha256 = %x, want the SHA-256 of the published object %x", sum, want)
	}
}
