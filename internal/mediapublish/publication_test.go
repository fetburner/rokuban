package mediapublish_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapublish"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

type cancelDuringCopyReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelDuringCopyReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		r.cancel()
		return copy(p, []byte("first")), nil
	}
	return copy(p, []byte("second")), io.EOF
}

func TestCopyWithContextStopsAfterCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	dst := &bytes.Buffer{}
	n, err := mediapublish.CopyWithContext(ctx, dst, &cancelDuringCopyReader{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mediapublish.CopyWithContext error = %v, want context.Canceled", err)
	}
	if n != int64(len("first")) || dst.String() != "first" {
		t.Fatalf("copied %d bytes %q, want only the first chunk (%d bytes)", n, dst.String(), len("first"))
	}
}

const stagedPrefixForTest = ".rokuban-media-asset-"

func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	testutil.RequireNoError(t, err, "os.ReadDir")
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagedPrefixForTest) {
			out = append(out, e.Name())
		}
	}
	return out
}

func activeThumbnailSize(t *testing.T, pool *pgxpool.Pool, recordingID int64) (int64, bool) {
	t.Helper()
	var size int64
	err := pool.QueryRow(context.Background(),
		`SELECT a.size_bytes FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		 WHERE a.recording_id = $1 AND a.kind = 'thumbnail' AND c.state = 'active'`,
		recordingID).Scan(&size)
	if errors.Is(err, pgx5.ErrNoRows) {
		return 0, false
	}
	testutil.RequireNoError(t, err, "reading active media asset size")
	return size, true
}

func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir")
	testutil.RequireNoError(t, os.WriteFile(full, content, 0o644), "write")
	dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = relPath
		fixtureParams.SizeBytes = int64(len(content))
	})
}

func publishThumbnailForTest(pool *pgxpool.Pool, mediaDir string, id int64, scratch string) (int64, bool, error) {
	// 呼び出し時点の active original を入力として計画した初回生成を模す。
	ctx := context.Background()
	objects, err := fsstore.New(mediaDir)
	if err != nil {
		return 0, false, err
	}
	publisher := pgstoretest.Media(pool)
	var inputID int64
	if orig, err := publisher.GetActiveOriginalMediaAsset(ctx, id); err == nil {
		inputID = orig.ID
	}
	generation, found, err := publisher.GetDerivedMediaAssetGeneration(ctx, id, store.AssetKindThumbnail)
	if err != nil {
		return 0, false, err
	}
	if found {
		generation++
	}
	relPath := "thumbnails/initial.jpg"
	if generation > 0 {
		relPath = "thumbnails/replacement.g1.jpg"
	}
	return mediapublish.PublishGeneratedObject(ctx, objects, publisher,
		store.PendingMediaAssetCopy{RecordingID: id, Kind: store.AssetKindThumbnail, RelPath: relPath, DerivedGeneration: generation},
		scratch, mediapublish.ObjectContentType(relPath), false, nil,
		func(size int64, sum [sha256.Size]byte) (bool, error) {
			return publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
				RecordingID: id, RelPath: relPath, SizeBytes: size, SHA256: sum[:], InputAssetID: inputID, PendingCopy: true,
			}, func() error { return nil })
		})
}

func writeScratchForTest(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scratch.jpg")
	testutil.RequireNoError(t, os.WriteFile(p, b, 0o644), "os.WriteFile")
	return p
}

// active な thumbnail が既にあるなら公開を飛ばし、canonical も行も staged file も変えない。
func TestPublishGeneratedMediaAssetSkipsWhenDerivedAlreadyActive(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, id, "sites/default/skip-active.m2ts", []byte("original"))
	relPath := "thumbnails/initial.jpg"
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(canonicalPath), 0o755), "os.MkdirAll")
	prior := []byte("committed by another attempt")
	testutil.RequireNoError(t, os.WriteFile(canonicalPath, prior, 0o644), "os.WriteFile")
	generation := int64(0)
	assetID, err := sqlcgen.New(pool).UpsertDerivedMediaAsset(context.Background(), sqlcgen.UpsertDerivedMediaAssetParams{Kind: "thumbnail",
		RecordingID: id, SizeBytes: int64(len(prior)),
		DerivedGeneration: generation,
	})
	testutil.RequireNoError(t, err, "seeding active thumbnail asset")
	_, err = sqlcgen.New(pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID,
		Store:        dbtest.StoreName,
		RelPath:      relPath,
		State:        "active",
	})
	testutil.RequireNoError(t, err, "seeding active thumbnail copy")

	size, published, err := publishThumbnailForTest(pool, mediaDir, id, writeScratchForTest(t, []byte("late attempt bytes")))
	testutil.RequireNoError(t, err, "publishThumbnailForTest")
	if published || size != 0 {
		t.Fatalf("published = %v size = %d, want false 0", published, size)
	}
	if got, _ := os.ReadFile(canonicalPath); !bytes.Equal(got, prior) {
		t.Errorf("canonical bytes = %q, want %q", got, prior)
	}
	pendingPath := filepath.Join(mediaDir, filepath.FromSlash("thumbnails/replacement.g1.jpg"))
	if _, err := os.Stat(pendingPath); err != nil {
		t.Errorf("unpublished pending generation is missing: %v", err)
	}
	var pendingState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT c.state FROM media_asset_copies c WHERE c.store = $1 AND c.rel_path = $2`,
		dbtest.StoreName, "thumbnails/replacement.g1.jpg").Scan(&pendingState), "reading skipped generation state")
	if pendingState != "pending" {
		t.Errorf("skipped generation state = %q, want pending", pendingState)
	}
	if s, ok := activeThumbnailSize(t, pool, id); !ok || s != int64(len(prior)) {
		t.Errorf("active size = %d (found=%v), want %d", s, ok, len(prior))
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}

// ffmpeg 実行中に原本が active でなくなった場合は新しい active 派生行を作らず、Put 済み key は pending に残す。
func TestPublishGeneratedMediaAssetSkipsWhenOriginalNoLongerActive(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, id, "sites/default/skip-orig.m2ts", []byte("original"))
	dbtest.SetRecordingMediaAssetState(t, pool, id, store.AssetKindOriginal, "deleting")

	_, published, err := publishThumbnailForTest(pool, mediaDir, id, writeScratchForTest(t, []byte("orphan bytes")))
	testutil.RequireNoError(t, err, "publishThumbnailForTest")
	if published {
		t.Fatal("published = true, want false")
	}
	key := "thumbnails/initial.jpg"
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(key))
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Errorf("unpublished pending object is missing: %v", err)
	}
	if _, ok := activeThumbnailSize(t, pool, id); ok {
		t.Error("active thumbnail row created for deleted recording")
	}
	var pendingState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT c.state FROM media_asset_copies c WHERE c.store = $1 AND c.rel_path = $2`,
		dbtest.StoreName, key).Scan(&pendingState), "reading skipped thumbnail state")
	if pendingState != "pending" {
		t.Errorf("skipped thumbnail state = %q, want pending", pendingState)
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}
