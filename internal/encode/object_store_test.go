package encode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// objectStoreName は S3 の派生物の置き場の名前。FS の原本の置き場（dbtest.StoreName）とは別にする。
const objectStoreName = "s3main"

// encode が ffmpeg の前に予約した世代は、公開前に止まっても再利用しない。
func TestObjectEncodeReservationNeverReusesAnInterruptedGeneration(t *testing.T) {
	t.Parallel()
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
	testutil.RequireNoError(t, reserve(1), "first reservation")
	if err := reserve(1); !errors.Is(err, store.ErrGenerationAdvanced) {
		t.Fatalf("reserving the generation of an interrupted attempt = %v, want ErrGenerationAdvanced", err)
	}
	testutil.RequireNoError(t, reserve(2), "reserving the next generation")
	state, err := env.media.GetEncodedPublishState(ctx, recordingID, profile, false, false)
	testutil.RequireNoError(t, err, "loading encode publish state")
	if state.MaxCopyGeneration == nil || *state.MaxCopyGeneration != 2 {
		t.Fatalf("MaxCopyGeneration = %v, want 2 so that the next plan skips used generations", state.MaxCopyGeneration)
	}
}

// 世代は挿入後に変えない。置き換えは新しい世代の行を足し、旧世代の行を書き換えない。
// 公開前に止まった encode の再試行は、新しい世代の key を使う。2 回目の出力が 1 回目と違っても、
// 1 回目の key は上書きされず、公開されるのは 2 回目の key だけである。
func TestObjectEncodeRetryAfterInterruptedPublishUsesNewGeneration(t *testing.T) {
	env := newObjectEnv(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, env.pool)
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName), "writing local media store marker")
	sourceRel := fmt.Sprintf("s3-retry-%d/source.m2ts", time.Now().UnixNano())
	prepareEncodeInput(t, env.pool, recordingID, mediaDir, sourceRel, []string{"h264"}, []byte("original source"))
	initialKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 0, false)
	testutil.RequireNoError(t, err, "building initial encoded key")
	retryKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 1, false)
	testutil.RequireNoError(t, err, "building retry encoded key")

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
	testutil.RequireNoError(t, worker.Work(ctx, args), "retrying encode")
	if env.copyState(t, retryKey) != "active" {
		t.Fatalf("retry copy state = %q, want active at %s", env.copyState(t, retryKey), retryKey)
	}
	if env.copyState(t, initialKey) != "pending" {
		t.Fatalf("interrupted copy state = %q, want it left pending for the reclaim", env.copyState(t, initialKey))
	}
	var generation int64
	var sum []byte
	if err := env.pool.QueryRow(ctx,
		`SELECT derived_generation, sha256 FROM media_assets WHERE recording_id = $1 AND kind = 'encoded'`,
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
	testutil.RequireNoError(t, err, "creating filesystem object store")
	pool := testutil.SetupDB(t)
	return objectEnv{
		pool:    pool,
		objects: objects,
		media:   pgstore.NewMediaWithStores(pool, objectStoreName, dbtest.StoreName, objectStoreName),
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
