package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

const stagedPrefixForTest = ".rokuban-media-asset-"

func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagedPrefixForTest) {
			out = append(out, e.Name())
		}
	}
	return out
}

func activeAssetSize(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string) (int64, bool) {
	t.Helper()
	var size int64
	err := pool.QueryRow(context.Background(),
		`SELECT size_bytes FROM media_assets WHERE recording_id = $1 AND kind = $2 AND state = 'active'`,
		recordingID, kind).Scan(&size)
	if errors.Is(err, pgx5.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return size, true
}

// assertWorkerPublishesUnderRelPathLock は worker（run）を rel_path lock を保持したまま
// 走らせ、lock 中は canonical が既存バイトのまま・active 行が無く、解放後に
// want とそのサイズが公開されることを検証する。
func assertWorkerPublishesUnderRelPathLock(
	t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64,
	kind, relPath string, run func() error, want []byte,
) {
	t.Helper()
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := []byte("previous orphan bytes")
	if err := os.WriteFile(canonicalPath, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_ = lock.Close()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- run() }()

	deadline := time.After(10 * time.Second)
	for len(stagedFiles(t, filepath.Dir(canonicalPath))) == 0 {
		select {
		case err := <-done:
			got, _ := os.ReadFile(canonicalPath)
			t.Fatalf("worker returned (err=%v) while rel_path lock was held; canonical = %q", err, got)
		case <-deadline:
			t.Fatal("worker did not stage while waiting for rel_path lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, prior) {
		t.Fatalf("canonical bytes changed while rel_path lock held: got %q, want %q", got, prior)
	}
	if _, ok := activeAssetSize(t, pool, recordingID, kind); ok {
		t.Fatalf("active %s row visible while rel_path lock held", kind)
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not finish after rel_path lock was released")
	}
	got, err = os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes = %q, want published %q", got, want)
	}
	if size, ok := activeAssetSize(t, pool, recordingID, kind); !ok || size != int64(len(want)) {
		t.Fatalf("active %s size = %d (found=%v), want %d", kind, size, ok, len(want))
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}

func TestThumbnailWorkerPublishesUnderRelPathLock(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := seedRecordingWithOriginal(t, pool, mediaDir, "sites/default/thumb-lock.m2ts", nil, []byte("original"))
	w := &ThumbnailWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(),
		FFmpeg: "ffmpeg", FFprobe: "ffprobe", runCmd: fakeThumbnailTools(t, 100.0)}
	assertWorkerPublishesUnderRelPathLock(t, pool, mediaDir, id, "thumbnail", thumbnailRelPath(id),
		func() error {
			return w.Work(context.Background(), &river.Job[ThumbnailJobArgs]{
				JobRow: &rivertype.JobRow{}, Args: ThumbnailJobArgs{RecordingID: id}})
		}, tinyJPEG)
}

func TestSeekTilesWorkerPublishesUnderRelPathLock(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := seedRecordingWithOriginal(t, pool, mediaDir, "sites/default/tiles-lock.m2ts", nil, []byte("original"))
	cmd := &countingRunCmd{duration: "100"}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	assertWorkerPublishesUnderRelPathLock(t, pool, mediaDir, id, "seek_tiles", seekTilesRelPath(id),
		func() error { return runSeekTilesJob(t, w, id) }, tinyJPEG)
}

func publishThumbnailForTest(pool *pgxpool.Pool, mediaDir string, id int64, scratch string) (int64, bool, error) {
	relPath := thumbnailRelPath(id)
	// 呼び出し時点の active original を入力として計画した初回生成を模す。
	var inputID int64
	if orig, err := sqlcgen.New(pool).GetActiveOriginalMediaAsset(context.Background(), id); err == nil {
		inputID = orig.ID
	}
	return publishGeneratedMediaAsset(context.Background(), pool, mediaDir, relPath, scratch,
		func(ctx context.Context, q *sqlcgen.Queries) (bool, error) {
			return skipThumbnailPlanPublish(ctx, q, id, nil, inputID)
		},
		func(ctx context.Context, q *sqlcgen.Queries, size int64) error {
			_, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
				RecordingID: id, RelPath: relPath, SizeBytes: size,
			})
			return err
		})
}

func writeScratchForTest(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scratch.jpg")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// active な thumbnail が既にあるなら公開を飛ばし、canonical も行も staged file も変えない。
func TestPublishGeneratedMediaAssetSkipsWhenDerivedAlreadyActive(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := seedRecordingWithOriginal(t, pool, mediaDir, "sites/default/skip-active.m2ts", nil, []byte("original"))
	relPath := thumbnailRelPath(id)
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := []byte("committed by another attempt")
	if err := os.WriteFile(canonicalPath, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).UpsertThumbnailMediaAsset(context.Background(), sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: id, RelPath: relPath, SizeBytes: int64(len(prior)),
	}); err != nil {
		t.Fatal(err)
	}

	size, published, err := publishThumbnailForTest(pool, mediaDir, id, writeScratchForTest(t, []byte("late attempt bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if published || size != 0 {
		t.Fatalf("published = %v size = %d, want false 0", published, size)
	}
	if got, _ := os.ReadFile(canonicalPath); !bytes.Equal(got, prior) {
		t.Errorf("canonical bytes = %q, want %q", got, prior)
	}
	if s, ok := activeAssetSize(t, pool, id, "thumbnail"); !ok || s != int64(len(prior)) {
		t.Errorf("active size = %d (found=%v), want %d", s, ok, len(prior))
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}

// ffmpeg 実行中に原本が active でなくなった場合は、新しい active 派生行も canonical も作らない。
func TestPublishGeneratedMediaAssetSkipsWhenOriginalNoLongerActive(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := seedRecordingWithOriginal(t, pool, mediaDir, "sites/default/skip-orig.m2ts", nil, []byte("original"))
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleting' WHERE recording_id = $1 AND kind = 'original'`, id); err != nil {
		t.Fatal(err)
	}

	_, published, err := publishThumbnailForTest(pool, mediaDir, id, writeScratchForTest(t, []byte("orphan bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("published = true, want false")
	}
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(thumbnailRelPath(id)))
	if _, err := os.Stat(canonicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canonical exists after skipped publish: %v", err)
	}
	if _, ok := activeAssetSize(t, pool, id, "thumbnail"); ok {
		t.Error("active thumbnail row created for deleted recording")
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}

func TestNewWorkerScratchDirIsUniqueForSameJobAndAttempt(t *testing.T) {
	root := t.TempDir()
	first, err := newWorkerScratchDir(root, "thumbnail", 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newWorkerScratchDir(root, "thumbnail", 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("scratch directory reused: %q", first)
	}
	for _, path := range []string{first, second} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Errorf("scratch path %q is not a directory: info=%v err=%v", path, info, err)
		}
	}
}
