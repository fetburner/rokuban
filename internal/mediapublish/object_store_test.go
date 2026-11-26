package mediapublish_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapublish"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

const objectStoreName = "s3main"

// objectEnv は S3 の置き場の代わりに fsstore を使う統合テストの環境である。
// 公開と削除の DB の振る舞いを測るので、object store は objstore.Store の契約を満たせば足りる。
// 実際の S3 との適合は s3conformance タグのテストが測る。
type objectEnv struct {
	pool    *pgxpool.Pool
	objects objstore.Store
	media   *pgstore.Media
}

type beforePutObjectStore struct {
	objstore.Store
	beforePut func(context.Context, string) error
}

func (s beforePutObjectStore) Put(ctx context.Context, key string, src io.Reader, size int64, checksum [32]byte, contentType string) error {
	if s.beforePut != nil {
		if err := s.beforePut(ctx, key); err != nil {
			return err
		}
	}
	return s.Store.Put(ctx, key, src, size, checksum, contentType)
}

type afterPutObjectStore struct {
	objstore.Store
	afterPut func(context.Context, string) error
}

func (s afterPutObjectStore) Put(ctx context.Context, key string, src io.Reader, size int64, checksum [32]byte, contentType string) error {
	if err := s.Store.Put(ctx, key, src, size, checksum, contentType); err != nil {
		return err
	}
	if s.afterPut != nil {
		return s.afterPut(ctx, key)
	}
	return nil
}

type blockedDeleteObjectStore struct {
	objstore.Store
	key     string
	started chan<- struct{}
	release <-chan struct{}
}

func (s blockedDeleteObjectStore) Delete(ctx context.Context, key string) error {
	if key == s.key {
		select {
		case s.started <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.Delete(ctx, key)
}

func newObjectEnv(t *testing.T) objectEnv {
	t.Helper()
	pool := testutil.SetupDB(t)
	objects, err := fsstore.New(t.TempDir())
	testutil.RequireNoError(t, err, "creating filesystem object store")
	return objectEnv{
		pool: pool, objects: objects,
		media: pgstore.NewMediaForDerivedStore(pool, objectStoreName, dbtest.StoreName, store.NewStoragePolicy(nil, nil, objectStoreName)),
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

// readObject は object の全体を読む。fsstore は Open、S3 は Get で読む。
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
	oldKey = e.publishThumbnail(t, recordingID, originalID, 0, "thumbnails/poster.jpg", nil, []byte("thumbnail generation zero"))
	var assetID int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`, recordingID).Scan(&assetID); err != nil {
		t.Fatalf("loading thumbnail asset: %v", err)
	}
	seek := int64(1000)
	newKey = e.publishThumbnail(t, recordingID, originalID, 1, "thumbnails/poster.g1.jpg",
		&store.ThumbnailObservation{ID: assetID, RelPath: oldKey, SeekMs: &seek, DerivedGeneration: 0},
		[]byte("thumbnail generation one"))
	return oldKey, newKey
}

// objectPublishOwnershipPublisher records whether cleanup asks the database who owns a key,
// and how many pending rows the publish path prepared.
type objectPublishOwnershipPublisher struct {
	store.DerivedMediaPublisher
	ownershipChecks int
	prepares        int
}

// PreparePendingMediaAssetCopy counts the call and succeeds, so a test can tell whether the
// publish path reserved a pending row before it checked the key.
func (p *objectPublishOwnershipPublisher) PreparePendingMediaAssetCopy(context.Context, store.PendingMediaAssetCopy) error {
	p.prepares++
	return nil
}

// HasLiveMediaAssetCopy records an ownership lookup; this conflict path must not need one.
func (p *objectPublishOwnershipPublisher) HasLiveMediaAssetCopy(context.Context, string) (bool, error) {
	p.ownershipChecks++
	return false, nil
}

// TestObjectPublishTreatsIdenticalExistingKeyAsAnotherPublisher fixes the ownership
// signal used by cleanup: an identical object already created by another Put is not ours.
func TestObjectPublishTreatsIdenticalExistingKeyAsAnotherPublisher(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base, err := fsstore.New(t.TempDir())
	testutil.RequireNoError(t, err, "creating filesystem object store")
	publisher := &objectPublishOwnershipPublisher{}
	const key = "thumbnails/earlier-publisher.jpg"
	payload := []byte("identical object from the earlier publisher")
	scratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
	writeFile(t, scratch, payload)
	pending := store.PendingMediaAssetCopy{
		RecordingID: 1, Kind: store.AssetKindThumbnail, RelPath: key,
		DerivedGeneration: 0, ReserveNewGeneration: true,
	}
	objects := beforePutObjectStore{
		Store: base,
		beforePut: func(putCtx context.Context, putKey string) error {
			if putKey != key {
				return fmt.Errorf("unexpected Put key %q", putKey)
			}
			return base.Put(putCtx, putKey, bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "image/jpeg")
		},
	}
	commitCalled := false
	_, published, err := mediapublish.PublishGeneratedObject(ctx, objects, publisher, pending, scratch, "image/jpeg", false, nil,
		func(int64, [sha256.Size]byte) (bool, error) {
			commitCalled = true
			return true, nil
		})
	if err != nil || published {
		t.Fatalf("publish result = (%t, %v), want an uncommitted no-op", published, err)
	}
	if commitCalled {
		t.Fatal("commit ran after Put reported that the key already existed")
	}
	if publisher.ownershipChecks != 0 {
		t.Fatalf("cleanup ownership checks = %d, want 0 for a key this Put did not create", publisher.ownershipChecks)
	}
	body, _, err := base.Open(ctx, key)
	testutil.RequireNoError(t, err, "opening the earlier publisher's object")
	got, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("reading the earlier publisher's object = (%v, %v)", readErr, closeErr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("object after the second publisher = %q, want the earlier bytes %q", got, payload)
	}
}

// TestObjectPublishFailsOnOrphanKeyWithoutLiveOwner は、DB 行の無い既存キーを
// pendingPrepared=false で公開すると失敗し、既存のバイトに触らないことを固定する。
// 相手の pending 行が無いキーを「公開中の相手」とみなして nil で抜けると、孤児を黙って残す。
func TestObjectPublishFailsOnOrphanKeyWithoutLiveOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base, err := fsstore.New(t.TempDir())
	testutil.RequireNoError(t, err, "creating filesystem object store")
	publisher := &objectPublishOwnershipPublisher{}
	const key = "thumbnails/orphan.jpg"
	orphan := []byte("orphan bytes with no database row")
	testutil.RequireNoError(t, base.Put(ctx, key, bytes.NewReader(orphan), int64(len(orphan)), sha256.Sum256(orphan), "image/jpeg"), "seeding orphan object")
	scratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
	writeFile(t, scratch, []byte("new generated bytes"))
	pending := store.PendingMediaAssetCopy{
		RecordingID: 1, Kind: store.AssetKindThumbnail, RelPath: key,
		DerivedGeneration: 0, ReserveNewGeneration: true,
	}
	commitCalled := false
	_, published, err := mediapublish.PublishGeneratedObject(ctx, base, publisher, pending, scratch, "image/jpeg", false, nil,
		func(int64, [sha256.Size]byte) (bool, error) {
			commitCalled = true
			return true, nil
		})
	if !errors.Is(err, objstore.ErrAlreadyExists) {
		t.Fatalf("publish error = %v, want ErrAlreadyExists for an orphan key", err)
	}
	if published || commitCalled {
		t.Fatalf("published = %t, commit called = %t; want neither for an orphan key", published, commitCalled)
	}
	if publisher.prepares != 0 {
		t.Fatalf("pending rows prepared = %d, want 0: an orphan key must be refused before any pending row names it", publisher.prepares)
	}
	body, _, err := base.Open(ctx, key)
	testutil.RequireNoError(t, err, "opening the orphan object")
	got, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("reading the orphan object = (%v, %v)", readErr, closeErr)
	}
	if !bytes.Equal(got, orphan) {
		t.Fatalf("orphan object = %q, want the untouched bytes %q", got, orphan)
	}
}

// SHA-256 は作り手が commit の tx で論理行へ書く。S3 の metadata にだけ載せると、
// 論理行は NULL のまま残り、転送の照合が使えない。
func TestObjectPublishWritesSHA256InTheCommitTransaction(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	profile := "h264"
	dbtest.ProcessingPolicy(t, env.pool, recordingID, func(p *dbtest.ProcessingPolicyParams) { p.EncodeProfiles = []string{profile} })
	if _, err := pgstoretest.Media(env.pool).Begin(ctx, recordingID, profile, jobs.EncodeAttemptLimit); err != nil {
		t.Fatalf("seeding running encode attempt: %v", err)
	}

	thumbnail := []byte("thumbnail bytes")
	env.publishThumbnail(t, recordingID, originalID, 0, "thumbnails/poster.jpg", nil, thumbnail)

	tiles := []byte("tile sheet bytes")
	tilesKey := "thumbnails/sheet_tiles.jpg"
	tilesPath := t.TempDir() + "/tiles.jpg"
	writeFile(t, tilesPath, tiles)
	_, published, err := mediapublish.PublishGeneratedObject(ctx, env.objects, env.media,
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
	_, published, err = mediapublish.PublishGeneratedObject(ctx, env.objects, env.media,
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

// A live Put that finishes after pending reclaim must not leave its canonical key unowned.
func TestObjectPublishCleansKeyWrittenAfterPendingCopyWasReclaimed(t *testing.T) {
	t.Parallel()
	for _, markTrashed := range []bool{false, true} {
		name := "activation_detects_missing_copy"
		if markTrashed {
			name = "early_skip_detects_missing_copy"
		}
		t.Run(name, func(t *testing.T) {
			env := newObjectEnv(t)
			ctx := context.Background()
			recordingID := dbtest.Recording(t, env.pool)
			originalID := dbtest.MediaAsset(t, env.pool, recordingID)
			key := "thumbnails/poster.jpg"
			pending := store.PendingMediaAssetCopy{
				RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
			}
			testutil.RequireNoError(t, env.media.PreparePendingMediaAssetCopy(ctx, pending), "reserving pending copy")
			var copyID int64
			testutil.RequireNoError(t, env.pool.QueryRow(ctx, `SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, objectStoreName, key).Scan(&copyID), "loading copy")
			testutil.MustExec(t, env.pool, "aging the copy", `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE id = $1`, copyID)
			scratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
			writeFile(t, scratch, []byte("thumbnail"))
			reclaimed := false
			objects := beforePutObjectStore{
				Store: env.objects,
				beforePut: func(putCtx context.Context, putKey string) error {
					if putKey != key {
						return fmt.Errorf("unexpected Put key %q", putKey)
					}
					var err error
					reclaimed, err = env.media.ReclaimPendingMediaAssetCopy(putCtx, copyID, time.Now(), func([]string) error {
						return env.objects.Delete(putCtx, key)
					})
					if err != nil || !markTrashed {
						return err
					}
					_, err = env.pool.Exec(putCtx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID)
					return err
				},
			}
			_, published, err := mediapublish.PublishGeneratedObject(ctx, objects, env.media, pending, scratch, "image/jpeg", true, nil,
				func(size int64, sum [sha256.Size]byte) (bool, error) {
					return env.media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
						RecordingID: recordingID, RelPath: key, SizeBytes: size, SeekMs: 1000, InputAssetID: originalID,
						SHA256: sum[:], PendingCopy: true,
					}, func() error { return nil })
				})
			if !reclaimed {
				t.Fatal("pending copy was not reclaimed before Put")
			}
			if published {
				t.Fatal("published after pending reclaim")
			}
			if markTrashed {
				if err != nil {
					t.Fatalf("publishing after a trashed recording skip = %v, want successful skip", err)
				}
			} else if !errors.Is(err, store.ErrStalePublishPlan) || !errors.Is(err, store.ErrPendingMediaAssetCopyMissing) {
				t.Fatalf("publishing after pending reclaim = %v, want stale-plan and missing-copy errors", err)
			}
			if env.objectExists(t, key) {
				t.Fatal("object written after pending reclaim remains without a live copy")
			}
			if got := env.copyState(t, key); got != "deleted" {
				t.Fatalf("reclaimed copy state = %q, want deleted", got)
			}
		})
	}
}

// A failed companion Put still cleans a main key whose pending row was reclaimed during Put.
func TestObjectPublishCleansMainKeyWhenCompanionPutFailsAfterReclaim(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	key := "thumbnails/poster.jpg"
	companionKey := "subtitles/reclaimed.vtt"
	pending := store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key,
	}
	mainScratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
	companionScratch := filepath.Join(t.TempDir(), "subtitle.vtt")
	writeFile(t, mainScratch, []byte("thumbnail"))
	writeFile(t, companionScratch, []byte("subtitle"))
	companionErr := errors.New("injected companion Put failure")
	reclaimed := false
	objects := beforePutObjectStore{
		Store: env.objects,
		beforePut: func(putCtx context.Context, putKey string) error {
			switch putKey {
			case key:
				var copyID int64
				if err := env.pool.QueryRow(putCtx,
					`SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2 AND state = 'pending'`, objectStoreName, key,
				).Scan(&copyID); err != nil {
					return fmt.Errorf("loading pending copy before Put: %w", err)
				}
				// 作成時刻を十分過去に固定し、DB と Go の時計差を除いて pending 回収を検証する。
				if _, err := env.pool.Exec(putCtx, `UPDATE media_asset_copies SET created_at = '2000-01-01T00:00:00Z'::timestamptz WHERE id = $1`, copyID); err != nil {
					return fmt.Errorf("aging pending copy before reclaim: %w", err)
				}
				var err error
				reclaimed, err = env.media.ReclaimPendingMediaAssetCopy(putCtx, copyID, time.Now(), func([]string) error { return nil })
				return err
			case companionKey:
				return companionErr
			default:
				return fmt.Errorf("unexpected Put key %q", putKey)
			}
		},
	}
	commitCalled := false
	_, published, err := mediapublish.PublishGeneratedObject(ctx, objects, env.media, pending, mainScratch, "image/jpeg", false, nil,
		func(int64, [sha256.Size]byte) (bool, error) {
			commitCalled = true
			return false, nil
		}, mediapublish.ObjectCompanion{Key: companionKey, Path: companionScratch, ContentType: "text/vtt"})
	if !reclaimed {
		t.Fatal("pending copy was not reclaimed during the main Put")
	}
	if published || !errors.Is(err, companionErr) || commitCalled {
		t.Fatalf("publish after companion failure = (published %t, err %v, commit called %t), want companion error without commit", published, err, commitCalled)
	}
	if env.objectExists(t, key) {
		t.Fatal("main object written after pending reclaim remains without a live copy")
	}
}

// A late cleanup delete must not remove a thumbnail generation published after reclaim.
func TestObjectPublishCleanupDoesNotDeleteNewerThumbnailGeneration(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	originalID := dbtest.MediaAsset(t, env.pool, recordingID)
	oldKey := env.publishThumbnail(t, recordingID, originalID, 0, "thumbnails/poster.jpg", nil, []byte("generation zero"))
	current, err := env.media.GetThumbnailMediaAssetForServing(ctx, recordingID)
	testutil.RequireNoError(t, err, "loading current thumbnail")
	seekMs := int64(1000)
	expected := &store.ThumbnailObservation{
		ID: current.ID, RelPath: current.RelPath, SeekMs: &seekMs, DerivedGeneration: 0,
	}

	keyA := "thumbnails/poster.g1.jpg"
	pendingA := store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: keyA,
		DerivedGeneration: 1, ReserveNewGeneration: true,
	}
	scratchA := filepath.Join(t.TempDir(), "thumbnail-a.jpg")
	writeFile(t, scratchA, []byte("replacement A"))
	cleanupStarted := make(chan struct{}, 1)
	releaseCleanup := make(chan struct{})
	var reclaimed bool
	var releaseOnce sync.Once
	releaseDelete := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }
	defer releaseDelete()
	objectsA := afterPutObjectStore{
		Store: blockedDeleteObjectStore{
			Store: env.objects, key: keyA, started: cleanupStarted, release: releaseCleanup,
		},
		afterPut: func(putCtx context.Context, putKey string) error {
			if putKey != keyA {
				return nil
			}
			var copyID int64
			if err := env.pool.QueryRow(putCtx,
				`SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2 AND state = 'pending'`, objectStoreName, keyA,
			).Scan(&copyID); err != nil {
				return fmt.Errorf("loading pending replacement before reclaim: %w", err)
			}
			// 作成時刻を十分過去に固定し、DB と Go の時計差を除いて pending 回収を検証する。
			if _, err := env.pool.Exec(putCtx, `UPDATE media_asset_copies SET created_at = '2000-01-01T00:00:00Z'::timestamptz WHERE id = $1`, copyID); err != nil {
				return fmt.Errorf("aging pending copy before reclaim: %w", err)
			}
			var err error
			reclaimed, err = env.media.ReclaimPendingMediaAssetCopy(putCtx, copyID, time.Now(), func([]string) error {
				return env.objects.Delete(putCtx, keyA)
			})
			return err
		},
	}
	type publishResult struct {
		published bool
		err       error
	}
	aDone := make(chan publishResult, 1)
	go func() {
		_, published, err := mediapublish.PublishGeneratedObject(ctx, objectsA, env.media, pendingA, scratchA, "image/jpeg", false, nil,
			func(size int64, sum [sha256.Size]byte) (bool, error) {
				return env.media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
					RecordingID: recordingID, RelPath: keyA, SizeBytes: size, SeekMs: 2000,
					InputAssetID: originalID, InputStore: dbtest.StoreName, DerivedGeneration: 1,
					Expected: expected, SHA256: sum[:], PendingCopy: true,
				}, func() error { return nil })
			})
		aDone <- publishResult{published: published, err: err}
	}()
	select {
	case <-cleanupStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("replacement A cleanup did not reach its delayed Delete")
	}
	if !reclaimed {
		t.Fatal("replacement A pending copy was not reclaimed after its Put")
	}
	if env.objectExists(t, keyA) {
		t.Fatal("pending reclaim did not remove replacement A before cleanup")
	}

	maxGeneration, found, err := env.media.GetDerivedMediaAssetGeneration(ctx, recordingID, "thumbnail")
	if err != nil || !found {
		t.Fatalf("loading thumbnail generation watermark = (%d, %t, %v), want an existing asset", maxGeneration, found, err)
	}
	generation := maxGeneration + 1
	if generation != 2 {
		t.Fatalf("next thumbnail generation = %d, want 2 after reclaiming generation 1", generation)
	}
	keyB := "thumbnails/poster.g2.jpg"
	scratchB := filepath.Join(t.TempDir(), "thumbnail-b.jpg")
	writeFile(t, scratchB, []byte("replacement B"))
	_, published, err := mediapublish.PublishGeneratedObject(ctx, env.objects, env.media, store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: keyB,
		DerivedGeneration: generation, ReserveNewGeneration: true,
	}, scratchB, "image/jpeg", false, nil, func(size int64, sum [sha256.Size]byte) (bool, error) {
		return env.media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
			RecordingID: recordingID, RelPath: keyB, SizeBytes: size, SeekMs: 3000,
			InputAssetID: originalID, InputStore: dbtest.StoreName, DerivedGeneration: generation,
			Expected: expected, SHA256: sum[:], PendingCopy: true,
		}, func() error { return nil })
	})
	if err != nil || !published {
		t.Fatalf("publishing replacement B at generation %d = (%t, %v), want published", generation, published, err)
	}

	releaseDelete()
	select {
	case result := <-aDone:
		if result.published || !errors.Is(result.err, store.ErrPendingMediaAssetCopyMissing) {
			t.Fatalf("replacement A publish = (%t, %v), want a reclaimed pending copy", result.published, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replacement A cleanup did not finish after releasing Delete")
	}
	if !env.objectExists(t, keyB) {
		t.Fatal("late cleanup of replacement A removed replacement B's active object")
	}
	active, err := env.media.GetThumbnailMediaAssetForServing(ctx, recordingID)
	if err != nil || active.RelPath != keyB {
		t.Fatalf("active thumbnail = (%+v, %v), want replacement B at %q", active, err, keyB)
	}
	if got := env.copyState(t, keyB); got != "active" {
		t.Fatalf("replacement B copy state = %q, want active", got)
	}
	if !env.objectExists(t, oldKey) {
		t.Fatal("expected original generation zero object to remain available")
	}
}

func TestObjectReplacementAddsRowAndNeverRewritesGeneration(t *testing.T) {
	t.Parallel()
	env := newObjectEnv(t)
	ctx := context.Background()
	oldKey, newKey := env.replaceThumbnail(t)
	rows, err := env.pool.Query(ctx, `SELECT rel_path, generation FROM media_asset_copies WHERE store = $1 ORDER BY id`, objectStoreName)
	testutil.RequireNoError(t, err, "listing copies")
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var key string
		var generation int64
		testutil.RequireNoError(t, rows.Scan(&key, &generation), "scanning copy")
		got[key] = generation
	}
	if len(got) != 2 || got[oldKey] != 0 || got[newKey] != 1 {
		t.Fatalf("copy generations = %v, want {%s: 0, %s: 1}", got, oldKey, newKey)
	}
}
