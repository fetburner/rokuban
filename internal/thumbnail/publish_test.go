package thumbnail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func assertMediaAssetCopy(t *testing.T, pool *pgxpool.Pool, assetID int64, wantRelPath string) {
	t.Helper()
	var gotStore, gotRelPath, gotState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT copy.store, copy.rel_path, copy.state
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = $1 AND copy.store = $2
  AND copy.generation = asset.derived_generation
  AND copy.state = 'active'`, assetID, dbtest.StoreName,
	).Scan(&gotStore, &gotRelPath, &gotState), fmt.Sprintf("reading media asset copy %d in store %q", assetID, dbtest.StoreName))
	if gotRelPath != wantRelPath || gotState != "active" {
		t.Fatalf("media asset copy = %q/%q/%q, want %s/%q/active", gotStore, gotRelPath, gotState, dbtest.StoreName, wantRelPath)
	}
}

func activeAssetSize(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string) (int64, bool) {
	t.Helper()
	var size int64
	err := pool.QueryRow(context.Background(),
		`SELECT a.size_bytes FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		 WHERE a.recording_id = $1 AND a.kind = $2 AND c.state = 'active'`,
		recordingID, kind).Scan(&size)
	if errors.Is(err, pgx5.ErrNoRows) {
		return 0, false
	}
	testutil.RequireNoError(t, err, "reading active media asset size")
	return size, true
}

func assertMediaAssetSHA256(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string, content []byte) {
	t.Helper()
	query := `SELECT sha256 FROM media_assets WHERE recording_id = $1 AND kind = $2 AND profile IS NULL AND EXISTS (SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = media_assets.id AND c.state = 'active')`
	var got []byte
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), query, recordingID, kind).Scan(&got), fmt.Sprintf("reading active %s media asset SHA-256 for recording %d", kind, recordingID))
	want := sha256.Sum256(content)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("active %s SHA-256 = %x, want %x", kind, got, want)
	}
}

func TestThumbnailWorkerPublishesThroughObjectStore(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, id, "sites/default/thumb-lock.m2ts", []byte("original"))
	deletedID := dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
		p.Kind = store.AssetKindThumbnail
		p.RelPath = thumbnailRelPath(id, 0)
	})
	testutil.MustExec(t, pool, "seeding deleted thumbnail copy", `
UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, deletedID)
	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		FFmpeg: "ffmpeg", FFprobe: "ffprobe", runCmd: fakeThumbnailTools(t, 100.0)}
	relPath := thumbnailRelPath(id, 1)
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailJobArgs{RecordingID: id}), "ThumbnailWorker.Work")
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if got, err := os.ReadFile(canonicalPath); err != nil || !bytes.Equal(got, tinyJPEG) {
		t.Fatalf("published thumbnail = (%q, %v), want %q", got, err, tinyJPEG)
	}
	if size, ok := activeAssetSize(t, pool, id, "thumbnail"); !ok || size != int64(len(tinyJPEG)) {
		t.Fatalf("active thumbnail size = %d (found=%v), want %d", size, ok, len(tinyJPEG))
	}
	assertMediaAssetSHA256(t, pool, id, "thumbnail", tinyJPEG)
	var assetID int64
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT a.id FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id WHERE a.recording_id = $1 AND a.kind = 'thumbnail' AND c.state = 'active'`, id).Scan(&assetID), "reading active thumbnail id")
	assertMediaAssetCopy(t, pool, assetID, relPath)
}

func TestThumbnailWorkerUsesAttemptUniqueScratchForOverlappingAttempts(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	id := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, id, "sites/default/thumb-overlap.m2ts", []byte("original"))

	paths := make(chan string, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: scratchDir, FFmpeg: "ffmpeg", FFprobe: "ffprobe",
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if strings.Contains(name, "ffprobe") || containsArg(args, "format=duration") {
				return []byte("100\n"), nil
			}
			if len(args) == 0 {
				return nil, errors.New("ffmpeg: no args")
			}
			out := args[len(args)-1]
			paths <- out
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(out, tinyJPEG, 0o644)
		},
	}

	for attempt := 3; attempt <= 4; attempt++ {
		job := jobs.ThumbnailJobArgs{RecordingID: id}
		go func(job jobs.ThumbnailJobArgs) {
			results <- jobqfaketest.RunWorkerOnce(context.Background(), w, job)
		}(job)
	}

	gotPaths := make([]string, 0, 2)
	deadline := time.After(10 * time.Second)
	pathsTimedOut := false
	for len(gotPaths) < 2 {
		select {
		case path := <-paths:
			gotPaths = append(gotPaths, path)
		case <-deadline:
			pathsTimedOut = true
		}
		if pathsTimedOut {
			break
		}
	}
	close(release)

	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("ThumbnailWorker.Work: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("overlapping thumbnail attempts did not finish after ffmpeg was released")
		}
	}
	if len(gotPaths) != 2 {
		t.Fatalf("ffmpeg output paths = %v, want both overlapping attempts to reach extraction", gotPaths)
	}
	if gotPaths[0] == gotPaths[1] {
		t.Fatalf("overlapping attempts share ffmpeg output path %q", gotPaths[0])
	}
}

func TestSeekTilesWorkerPublishesThroughObjectStore(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, id, "sites/default/tiles-lock.m2ts", []byte("original"))
	cmd := &countingRunCmd{duration: "100"}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	testutil.RequireNoError(t, runSeekTilesJob(t, w, id), "SeekTilesWorker.Work")
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(seekTilesRelPath(id, 0)))
	if got, err := os.ReadFile(canonicalPath); err != nil || !bytes.Equal(got, tinyJPEG) {
		t.Fatalf("published seek tiles = (%q, %v), want %q", got, err, tinyJPEG)
	}
	if size, ok := activeAssetSize(t, pool, id, "seek_tiles"); !ok || size != int64(len(tinyJPEG)) {
		t.Fatalf("active seek_tiles size = %d (found=%v), want %d", size, ok, len(tinyJPEG))
	}
	assertMediaAssetSHA256(t, pool, id, "seek_tiles", tinyJPEG)
}
