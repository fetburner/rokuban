package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

type cancelDuringCopyReader struct {
	cancel context.CancelFunc
	reads  int
}

func assertMediaAssetCopy(t *testing.T, pool *pgxpool.Pool, assetID int64, wantRelPath string) {
	t.Helper()
	var gotStore, gotRelPath, gotState string
	if err := pool.QueryRow(context.Background(), `
SELECT copy.store, copy.rel_path, copy.state
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = $1 AND copy.store = $2
  AND copy.generation = COALESCE(asset.derived_generation, 0)
  AND copy.state = 'active'`, assetID, dbtest.StoreName,
	).Scan(&gotStore, &gotRelPath, &gotState); err != nil {
		t.Fatalf("reading media asset copy %d in store %q: %v", assetID, dbtest.StoreName, err)
	}
	if gotRelPath != wantRelPath || gotState != "active" {
		t.Fatalf("media asset copy = %q/%q/%q, want %s/%q/active", gotStore, gotRelPath, gotState, dbtest.StoreName, wantRelPath)
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	dst := &bytes.Buffer{}
	n, err := copyWithContext(ctx, dst, &cancelDuringCopyReader{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyWithContext error = %v, want context.Canceled", err)
	}
	if n != int64(len("first")) || dst.String() != "first" {
		t.Fatalf("copied %d bytes %q, want only the first chunk (%d bytes)", n, dst.String(), len("first"))
	}
}

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
		`SELECT a.size_bytes FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		 WHERE a.recording_id = $1 AND a.kind = $2 AND c.state = 'active'`,
		recordingID, kind).Scan(&size)
	if errors.Is(err, pgx5.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return size, true
}

func assertMediaAssetSHA256(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind, profile string, content []byte) {
	t.Helper()
	query := `SELECT sha256 FROM media_assets WHERE recording_id = $1 AND kind = $2 AND EXISTS (SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = media_assets.id AND c.state = 'active')`
	args := []any{recordingID, kind}
	if profile == "" {
		query += ` AND profile IS NULL`
	} else {
		query += ` AND profile = $3`
		args = append(args, profile)
	}
	var got []byte
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("reading active %s media asset SHA-256 for recording %d: %v", kind, recordingID, err)
	}
	want := sha256.Sum256(content)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("active %s SHA-256 = %x, want %x", kind, got, want)
	}
}

func TestThumbnailWorkerPublishesThroughObjectStore(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "sites/default/thumb-lock.m2ts", nil, []byte("original"))
	deletedID := dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
		p.Kind = store.AssetKindThumbnail
		p.RelPath = thumbnailRelPath(id, 0)
	})
	if _, err := pool.Exec(context.Background(), `
UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, deletedID); err != nil {
		t.Fatalf("seeding deleted thumbnail copy: %v", err)
	}
	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		FFmpeg: "ffmpeg", FFprobe: "ffprobe", runCmd: fakeThumbnailTools(t, 100.0)}
	relPath := thumbnailRelPath(id, 1)
	if err := runWorkerViaJobQFake(context.Background(), w, ThumbnailJobArgs{RecordingID: id}); err != nil {
		t.Fatalf("ThumbnailWorker.Work: %v", err)
	}
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if got, err := os.ReadFile(canonicalPath); err != nil || !bytes.Equal(got, tinyJPEG) {
		t.Fatalf("published thumbnail = (%q, %v), want %q", got, err, tinyJPEG)
	}
	if size, ok := activeAssetSize(t, pool, id, "thumbnail"); !ok || size != int64(len(tinyJPEG)) {
		t.Fatalf("active thumbnail size = %d (found=%v), want %d", size, ok, len(tinyJPEG))
	}
	assertMediaAssetSHA256(t, pool, id, "thumbnail", "", tinyJPEG)
	var assetID int64
	if err := pool.QueryRow(context.Background(), `SELECT a.id FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id WHERE a.recording_id = $1 AND a.kind = 'thumbnail' AND c.state = 'active'`, id).Scan(&assetID); err != nil {
		t.Fatalf("reading active thumbnail id: %v", err)
	}
	assertMediaAssetCopy(t, pool, assetID, relPath)
}

func TestThumbnailWorkerUsesAttemptUniqueScratchForOverlappingAttempts(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "sites/default/thumb-overlap.m2ts", nil, []byte("original"))

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
		job := ThumbnailJobArgs{RecordingID: id}
		go func(job ThumbnailJobArgs) {
			results <- runWorkerViaJobQFake(context.Background(), w, job)
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
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "sites/default/tiles-lock.m2ts", nil, []byte("original"))
	cmd := &countingRunCmd{duration: "100"}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, id); err != nil {
		t.Fatalf("SeekTilesWorker.Work: %v", err)
	}
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(seekTilesRelPath(id, 0)))
	if got, err := os.ReadFile(canonicalPath); err != nil || !bytes.Equal(got, tinyJPEG) {
		t.Fatalf("published seek tiles = (%q, %v), want %q", got, err, tinyJPEG)
	}
	if size, ok := activeAssetSize(t, pool, id, "seek_tiles"); !ok || size != int64(len(tinyJPEG)) {
		t.Fatalf("active seek_tiles size = %d (found=%v), want %d", size, ok, len(tinyJPEG))
	}
	assertMediaAssetSHA256(t, pool, id, "seek_tiles", "", tinyJPEG)
}

func publishThumbnailForTest(pool *pgxpool.Pool, mediaDir string, id int64, scratch string) (int64, bool, error) {
	// 呼び出し時点の active original を入力として計画した初回生成を模す。
	ctx := context.Background()
	objects := testFSObjectStore(mediaDir)
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
	pathPicker := ThumbnailWorker{Objects: objects}
	relPath, generation, err := pathPicker.nextUnusedThumbnailRelPath(ctx, id, generation)
	if err != nil {
		return 0, false, err
	}
	return publishGeneratedObject(ctx, objects, publisher,
		store.PendingMediaAssetCopy{RecordingID: id, Kind: store.AssetKindThumbnail, RelPath: relPath, DerivedGeneration: generation},
		scratch, objectContentType(relPath), false, nil,
		func(size int64, sum [sha256.Size]byte) (bool, error) {
			return publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
				RecordingID: id, RelPath: relPath, SizeBytes: size, SHA256: sum[:], InputAssetID: inputID, PendingCopy: true,
			}, func() error { return nil })
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
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "sites/default/skip-active.m2ts", nil, []byte("original"))
	relPath := thumbnailRelPath(id, 0)
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := []byte("committed by another attempt")
	if err := os.WriteFile(canonicalPath, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	assetID, err := sqlcgen.New(pool).UpsertDerivedMediaAsset(context.Background(), sqlcgen.UpsertDerivedMediaAssetParams{Kind: "thumbnail",
		RecordingID: id, SizeBytes: int64(len(prior)),
		DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID,
		Store:        dbtest.StoreName,
		RelPath:      relPath,
		State:        "active",
	}); err != nil {
		t.Fatalf("seeding active thumbnail copy: %v", err)
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
	pendingPath := filepath.Join(mediaDir, filepath.FromSlash(thumbnailRelPath(id, 1)))
	if _, err := os.Stat(pendingPath); err != nil {
		t.Errorf("unpublished pending generation is missing: %v", err)
	}
	var pendingState string
	if err := pool.QueryRow(context.Background(), `SELECT c.state FROM media_asset_copies c WHERE c.store = $1 AND c.rel_path = $2`,
		dbtest.StoreName, thumbnailRelPath(id, 1)).Scan(&pendingState); err != nil {
		t.Fatalf("reading skipped generation state: %v", err)
	}
	if pendingState != "pending" {
		t.Errorf("skipped generation state = %q, want pending", pendingState)
	}
	if s, ok := activeAssetSize(t, pool, id, "thumbnail"); !ok || s != int64(len(prior)) {
		t.Errorf("active size = %d (found=%v), want %d", s, ok, len(prior))
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}

// ffmpeg 実行中に原本が active でなくなった場合は新しい active 派生行を作らず、Put 済み key は pending に残す。
func TestPublishGeneratedMediaAssetSkipsWhenOriginalNoLongerActive(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "sites/default/skip-orig.m2ts", nil, []byte("original"))
	dbtest.SetRecordingMediaAssetState(t, pool, id, store.AssetKindOriginal, "deleting")

	_, published, err := publishThumbnailForTest(pool, mediaDir, id, writeScratchForTest(t, []byte("orphan bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("published = true, want false")
	}
	key := thumbnailRelPath(id, 0)
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(key))
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Errorf("unpublished pending object is missing: %v", err)
	}
	if _, ok := activeAssetSize(t, pool, id, "thumbnail"); ok {
		t.Error("active thumbnail row created for deleted recording")
	}
	var pendingState string
	if err := pool.QueryRow(context.Background(), `SELECT c.state FROM media_asset_copies c WHERE c.store = $1 AND c.rel_path = $2`,
		dbtest.StoreName, key).Scan(&pendingState); err != nil {
		t.Fatalf("reading skipped thumbnail state: %v", err)
	}
	if pendingState != "pending" {
		t.Errorf("skipped thumbnail state = %q, want pending", pendingState)
	}
	if left := stagedFiles(t, filepath.Dir(canonicalPath)); len(left) != 0 {
		t.Errorf("staged files remain: %v", left)
	}
}

func TestRemoveStaleEncodeScratchScopesByRecordingAndProfile(t *testing.T) {
	root := t.TempDir()
	makeDir := func(rel string) string {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale1 := makeDir(filepath.Join("encode", encodeScratchScope(7, "h264")+"-1-old"))
	stale2 := makeDir(filepath.Join("encode", encodeScratchScope(7, "h264")+"-2-old"))
	otherProfile := makeDir(filepath.Join("encode", encodeScratchScope(7, "cut")+"-1-other"))
	otherRecording := makeDir(filepath.Join("encode", encodeScratchScope(70, "h264")+"-1-other"))
	otherKind := makeDir("thumbnail/7-other")

	removeStaleEncodeScratch(root, 7, "h264", slog.Default())

	for _, path := range []string{stale1, stale2} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stale scratch %s stat error = %v, want os.ErrNotExist", path, err)
		}
	}
	for _, path := range []string{otherProfile, otherRecording, otherKind} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated scratch %s was removed: %v", path, err)
		}
	}
}

// 生きた試行は同じ scope でも残し、死んだ兄弟は消す。生きた試行は attempt 2 で、
// os.ReadDir の名前順では -1-dead < -2-<random> < -3-dead になる。生きた試行に
// 当たっても走査を打ち切らず、後ろの -3-dead も消えることを見る。
func TestRemoveStaleEncodeScratchKeepsLiveAttemptOfSameScope(t *testing.T) {
	root := t.TempDir()
	live, liveLock, err := newEncodeScratchDir(root, 7, "h264", 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeEncodeScratchLock(liveLock) })
	var dead []string
	for _, name := range []string{"-1-dead", "-3-dead"} {
		d := filepath.Join(root, "encode", encodeScratchScope(7, "h264")+name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		dead = append(dead, d)
	}

	removeStaleEncodeScratch(root, 7, "h264", slog.Default())

	if _, err := os.Stat(live); err != nil {
		t.Errorf("live attempt of the same scope was removed: %v", err)
	}
	for _, d := range dead {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dead sibling %s stat error = %v, want os.ErrNotExist", filepath.Base(d), err)
		}
	}
	if err := closeEncodeScratchLock(liveLock); err != nil {
		t.Fatal(err)
	}
	removeStaleEncodeScratch(root, 7, "h264", slog.Default())
	if _, err := os.Stat(live); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("released attempt stat error = %v, want os.ErrNotExist", err)
	}
}

// 別の recording / profile の生きた encode があっても、対象 scope の死んだ残骸は消える。
func TestRemoveStaleEncodeScratchIgnoresLiveEncodeOfOtherScope(t *testing.T) {
	root := t.TempDir()
	otherRecording, l1, err := newEncodeScratchDir(root, 70, "h264", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeEncodeScratchLock(l1) })
	otherProfile, l2, err := newEncodeScratchDir(root, 7, "cut", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeEncodeScratchLock(l2) })
	dead := filepath.Join(root, "encode", encodeScratchScope(7, "h264")+"-1-dead")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}

	removeStaleEncodeScratch(root, 7, "h264", slog.Default())

	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dead scratch stat error = %v, want os.ErrNotExist (blocked by another scope's live encode)", err)
	}
	for _, path := range []string{otherRecording, otherProfile} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("live scratch %s was removed: %v", path, err)
		}
	}
}

// 作成（MkdirTemp から dir lock 取得まで）の間は掃除が入れない。gate を外から
// 保持して、掃除が待つこと・解放後に進むことを見る。
func TestRemoveStaleEncodeScratchWaitsForScratchCreationGate(t *testing.T) {
	root := t.TempDir()
	dead := filepath.Join(root, "encode", encodeScratchScope(7, "h264")+"-1-dead")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	gate, err := openEncodeScratchGate(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := flockScratch(gate, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		removeStaleEncodeScratch(root, 7, "h264", slog.Default())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("cleanup ran while the creation gate was held")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(dead); err != nil {
		t.Errorf("dead scratch removed while gate held: %v", err)
	}
	if err := closeEncodeScratchLock(gate); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not proceed after the gate was released")
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dead scratch stat error = %v, want os.ErrNotExist", err)
	}
}

// 作成側も gate を取る。掃除側の待ちと合わせて、作成直後で lock 前の dir が消される窓が塞がる。
func TestNewEncodeScratchDirWaitsForScratchCreationGate(t *testing.T) {
	root := t.TempDir()
	gate, err := openEncodeScratchGate(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := flockScratch(gate, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	type result struct {
		lock *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, lock, err := newEncodeScratchDir(root, 7, "h264", 1)
		done <- result{lock, err}
	}()
	select {
	case <-done:
		t.Fatal("scratch creation ran while the gate was held")
	case <-time.After(200 * time.Millisecond):
	}
	if err := closeEncodeScratchLock(gate); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		_ = closeEncodeScratchLock(r.lock)
	case <-time.After(5 * time.Second):
		t.Fatal("scratch creation did not proceed after the gate was released")
	}
}

func TestNewEncodeScratchDirUsesRecordingProfileAndAttempt(t *testing.T) {
	root := t.TempDir()
	dir, lock, err := newEncodeScratchDir(root, 42, "h264", 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeEncodeScratchLock(lock) })

	wantPrefix := filepath.Join(root, "encode", "42-72a6c9135f1b59693330c8ae39e10e2c1fcc87a6c40865ca66b59d24dadacf60-3-")
	if !strings.HasPrefix(dir, wantPrefix) {
		t.Errorf("encode scratch directory = %q, want prefix %q", dir, wantPrefix)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("encode scratch directory stat = (%v, %v), want directory", info, err)
	}
}

func TestNewEncodeScratchDirSupportsLongProfileNames(t *testing.T) {
	root := t.TempDir()
	profile := strings.Repeat("profile", 128)
	dir, lock, err := newEncodeScratchDir(root, 42, profile, 3)
	if err != nil {
		t.Fatalf("newEncodeScratchDir with long profile name: %v", err)
	}
	t.Cleanup(func() { _ = closeEncodeScratchLock(lock) })
	if got, want := filepath.Base(dir), fmt.Sprintf("42-%x-3-", sha256.Sum256([]byte(profile))); !strings.HasPrefix(got, want) {
		t.Errorf("encode scratch directory = %q, want prefix %q", got, want)
	}
	if got := len(filepath.Base(dir)); got > 255 {
		t.Errorf("encode scratch basename is %d bytes, want at most 255", got)
	}
}

func TestNewWorkerScratchDirIsUniqueForSameRecording(t *testing.T) {
	root := t.TempDir()
	first, err := newWorkerScratchDir(root, "thumbnail", "42-")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newWorkerScratchDir(root, "thumbnail", "42-")
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
			make: func() (string, error) { return newWorkerScratchDir("", "thumbnail", "42-") },
		},
		{
			name: "domain-fixed",
			make: func() (string, error) { return newDomainScratchDir("", "cm_detect", "42-1") },
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

func TestNewDomainScratchDirClearsPreviousResidue(t *testing.T) {
	root := t.TempDir()
	dir, err := newDomainScratchDir(root, "cm_detect", "42-1")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "stale-output")
	if err := os.WriteFile(marker, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	otherJobDir, err := newDomainScratchDir(root, "cm_detect", "43-1")
	if err != nil {
		t.Fatal(err)
	}
	if otherJobDir == dir {
		t.Fatalf("different jobs share scratch directory %q", dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("creating another job's scratch directory changed the first job's residue: %v", err)
	}

	restartedDir, err := newDomainScratchDir(root, "cm_detect", "42-1")
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
