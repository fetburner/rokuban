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

func TestThumbnailWorkerUsesAttemptUniqueScratchForOverlappingAttempts(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	id := seedRecordingWithOriginal(t, pool, mediaDir, "sites/default/thumb-overlap.m2ts", nil, []byte("original"))

	paths := make(chan string, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	w := &ThumbnailWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir, FFmpeg: "ffmpeg", FFprobe: "ffprobe",
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
		job := &river.Job[ThumbnailJobArgs]{
			JobRow: &rivertype.JobRow{ID: 42, Attempt: attempt},
			Args:   ThumbnailJobArgs{RecordingID: id},
		}
		go func(job *river.Job[ThumbnailJobArgs]) {
			results <- w.Work(context.Background(), job)
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

func TestScratchDirectoryHelpersRejectEmptyRoot(t *testing.T) {
	cases := []struct {
		name string
		make func() (string, error)
	}{
		{
			name: "attempt-unique",
			make: func() (string, error) { return newWorkerScratchDir("", "thumbnail", 42, 3) },
		},
		{
			name: "job-fixed",
			make: func() (string, error) { return newJobScratchDir("", "encode", 42) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if dir, err := tc.make(); err == nil || err.Error() != "scratch dir is empty" {
				t.Fatalf("scratch directory = %q, error = %v, want empty-root error", dir, err)
			}
		})
	}
}

func TestNewJobScratchDirClearsPreviousResidue(t *testing.T) {
	root := t.TempDir()
	dir, err := newJobScratchDir(root, "cm_detect", 42)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "stale-output")
	if err := os.WriteFile(marker, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	otherJobDir, err := newJobScratchDir(root, "cm_detect", 43)
	if err != nil {
		t.Fatal(err)
	}
	if otherJobDir == dir {
		t.Fatalf("different jobs share scratch directory %q", dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("creating another job's scratch directory changed the first job's residue: %v", err)
	}

	restartedDir, err := newJobScratchDir(root, "cm_detect", 42)
	if err != nil {
		t.Fatal(err)
	}
	if restartedDir != dir {
		t.Fatalf("restarted scratch = %q, want stable path %q", restartedDir, dir)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("stale output stat error = %v, want os.ErrNotExist", err)
	}
}
