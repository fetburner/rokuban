package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// tinyJPEG は最小限の有効な JPEG（SOI + EOI）。fake ffmpeg が書き出す。
var tinyJPEG = []byte{0xFF, 0xD8, 0xFF, 0xD9}

func TestThumbnailSeek(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		want time.Duration
	}{
		{"zero", 0, 0},
		{"short 60s → 6s", 60 * time.Second, 6 * time.Second},
		{"10min → capped 30s", 10 * time.Minute, 30 * time.Second},
		{"exactly 300s → 30s", 300 * time.Second, 30 * time.Second},
		{"200s → 20s", 200 * time.Second, 20 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := thumbnailSeek(tt.dur)
			if got != tt.want {
				t.Errorf("thumbnailSeek(%v) = %v, want %v", tt.dur, got, tt.want)
			}
		})
	}
}

func TestThumbnailWorker_CreatesAsset(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	// 原本ファイル + media_assets 行。
	origRel := "shows/ep1.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	if err := os.MkdirAll(filepath.Dir(origPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origPath, []byte("fake-ts-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     origRel,
		SizeBytes:   int64(len("fake-ts-content")),
	}); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	w := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     "ffmpeg",
		FFprobe:    "ffprobe",
		runCmd:     fakeThumbnailTools(t, 100.0),
	}

	job := &river.Job[ThumbnailJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ThumbnailJobArgs{RecordingID: recordingID},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// DB 行が 1 つ。
	id, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("thumbnail asset missing: %v", err)
	}
	if id == 0 {
		t.Fatal("thumbnail asset id is 0")
	}

	// メディア上に JPEG がある。
	dest := filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d.jpg", recordingID))
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading thumbnail file: %v", err)
	}
	if !bytesEqual(data, tinyJPEG) {
		t.Errorf("thumbnail content = %v, want tinyJPEG", data)
	}

	// scratch は掃除されている。
	scratch := filepath.Join(scratchDir, "thumbnail", fmt.Sprintf("%d.jpg", recordingID))
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch file still exists: %v", err)
	}
}

func TestThumbnailWorker_IdempotentRerun(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	origRel := "shows/ep2.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	if err := os.MkdirAll(filepath.Dir(origPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origPath, []byte("ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     origRel,
		SizeBytes:   2,
	}); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	var ffmpegCalls int
	w := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if strings.Contains(name, "ffprobe") || (len(args) > 0 && containsArg(args, "format=duration")) {
				return []byte("60.0\n"), nil
			}
			// ffmpeg
			ffmpegCalls++
			out := args[len(args)-1]
			if err := os.WriteFile(out, tinyJPEG, 0o644); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}

	job := &river.Job[ThumbnailJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ThumbnailJobArgs{RecordingID: recordingID},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("first Work: %v", err)
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("second Work: %v", err)
	}

	// 2 回目は active thumbnail を見てスキップするので ffmpeg は 1 回だけ。
	if ffmpegCalls != 1 {
		t.Errorf("ffmpeg calls = %d, want 1 (second run must skip)", ffmpegCalls)
	}

	// 行は 1 つだけ。
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`,
		recordingID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("thumbnail rows = %d, want 1", n)
	}
}

func TestThumbnailWorker_SkipsWithoutOriginal(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
	w := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   t.TempDir(),
		ScratchDir: t.TempDir(),
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			t.Fatal("runCmd must not be called when original is missing")
			return nil, nil
		},
	}

	job := &river.Job[ThumbnailJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ThumbnailJobArgs{RecordingID: recordingID},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
}

func TestThumbnailWorker_CommitUpsertsExistingActiveRow(t *testing.T) {
	// commit は ON CONFLICT DO UPDATE の UpsertThumbnailMediaAsset を使うため、
	// 既に active な行がある recording に対して呼んでもエラーにならず
	// 同じ行を更新する（行数は増えない）。
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	origRel := "shows/race.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	if err := os.MkdirAll(filepath.Dir(origPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origPath, []byte("ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     origRel,
		SizeBytes:   2,
	}); err != nil {
		t.Fatal(err)
	}

	// 先に active な thumbnail 行を入れておく（別経路で先に着地したケースを模す）。
	rel := thumbnailRelPath(recordingID)
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindThumbnail,
		RelPath:     rel,
		SizeBytes:   int64(len(tinyJPEG)),
	}); err != nil {
		t.Fatalf("seed existing thumbnail: %v", err)
	}

	// commit 経路も成功する（DO UPDATE なので ErrNoRows にならない）。
	w := &ThumbnailWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir}
	if err := w.commit(context.Background(), recordingID, rel, int64(len(tinyJPEG))); err != nil {
		t.Fatalf("commit over existing active row: %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`,
		recordingID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("thumbnail rows = %d, want 1 (upsert must not duplicate)", n)
	}
}

func TestThumbnailWorker_RevivesTombstone(t *testing.T) {
	// state='deleted' の tombstone（過去の完全削除の残骸）がある recording に
	// 対して Work を実行すると、行が active に戻り GetActiveThumbnailMediaAssetID
	// が引けるようになる（issue #108）。
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	origRel := "shows/ep3.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	if err := os.MkdirAll(filepath.Dir(origPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origPath, []byte("ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     origRel,
		SizeBytes:   2,
	}); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	// tombstone を作る: いったん active な thumbnail 行を作り、削除プロトコル
	// の最終状態（state='deleted', deleted_at 有り）に直接遷移させる。
	rel := thumbnailRelPath(recordingID)
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindThumbnail,
		RelPath:     rel,
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE recording_id = $1 AND kind = 'thumbnail'`,
		recordingID,
	); err != nil {
		t.Fatalf("marking tombstone deleted: %v", err)
	}

	// tombstone がある状態では GetActiveThumbnailMediaAssetID は空を返す。
	if _, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID); !errors.Is(err, pgx5.ErrNoRows) {
		t.Fatalf("precondition: active thumbnail err = %v, want ErrNoRows", err)
	}

	w := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		runCmd:     fakeThumbnailTools(t, 100.0),
	}

	job := &river.Job[ThumbnailJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ThumbnailJobArgs{RecordingID: recordingID},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// tombstone が active に戻り、GetActiveThumbnailMediaAssetID が引ける。
	id, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("thumbnail asset not revived to active: %v", err)
	}
	if id == 0 {
		t.Fatal("thumbnail asset id is 0")
	}

	// 新規行を積まず、tombstone 行そのものを書き換えている（行は 1 つだけ）。
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`,
		recordingID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("thumbnail rows = %d, want 1 (tombstone should be revived, not duplicated)", n)
	}
}

func TestEnqueueThumbnailIfNeeded(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	// insert-only ではなく Workers 付きクライアント（Insert の Kind 登録用）。
	workers := river.NewWorkers()
	river.AddWorker(workers, &ThumbnailWorker{})
	client, err := NewClient(pool, workers, ClientConfig{PeriodicJobs: false})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	recordingID := insertTestRecording(t, pool)

	// original 無し → no-op。
	if err := EnqueueThumbnailIfNeeded(context.Background(), pool, client, recordingID); err != nil {
		t.Fatalf("enqueue without original: %v", err)
	}

	// original を置く。
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "x.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatal(err)
	}

	if err := EnqueueThumbnailIfNeeded(context.Background(), pool, client, recordingID); err != nil {
		t.Fatalf("enqueue with original: %v", err)
	}
	// 2 回目も UniqueOpts で合流（エラーにしない）。
	if err := EnqueueThumbnailIfNeeded(context.Background(), pool, client, recordingID); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	// thumbnail 行を作ると以降は no-op（ジョブを積まない）。
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindThumbnail,
		RelPath:     thumbnailRelPath(recordingID),
		SizeBytes:   1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := EnqueueThumbnailIfNeeded(context.Background(), pool, client, recordingID); err != nil {
		t.Fatalf("enqueue with thumbnail: %v", err)
	}
}

// TestEnqueueThumbnailIfNeeded_ExcludesTrash は issue #109 の回帰テスト。
// SoftDeleteRecording に status ガードは無く、ingest 進行中の録画もごみ箱に
// 入れられる（internal/db/queries/recordings_trash.sql）。original コミット
// 直後のヒント投入がその窓を踏んだ場合でも、ごみ箱の録画にはジョブを積まない
// （ListRecordingIDsMissingThumbnail と条件を揃える。docs/storage.md §5.1）。
func TestEnqueueThumbnailIfNeeded_ExcludesTrash(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	workers := river.NewWorkers()
	river.AddWorker(workers, &ThumbnailWorker{})
	client, err := NewClient(pool, workers, ClientConfig{PeriodicJobs: false})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	recordingID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "trashed3.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed original: %v", err)
	}
	if _, err := q.SoftDeleteRecording(ctx, recordingID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if err := EnqueueThumbnailIfNeeded(ctx, pool, client, recordingID); err != nil {
		t.Fatalf("enqueue for trashed recording: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'thumbnail' AND (args->>'recording_id')::bigint = $1`,
		recordingID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("river_job rows for trashed recording = %d, want 0", n)
	}
}

// TestListRecordingIDsMissingThumbnail_ExcludesTrash は issue #109 の回帰テスト。
// ごみ箱（recordings.deleted_at IS NOT NULL）の録画は、original があり
// thumbnail が無くても投入対象から外れる。生成しても配信側
// （GetThumbnailMediaAssetForServing）が r.deleted_at IS NULL を要求するため
// 誰にも配られず、ffmpeg の無駄打ちになるだけだからである。
func TestListRecordingIDsMissingThumbnail_ExcludesTrash(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	q := sqlcgen.New(pool)
	ctx := context.Background()

	// insertTestRecording は固定の network/service/event ID を使うため、
	// deleted_at IS NULL の行が 2 つ同時に存在すると unique partial index
	// (site, network_id, service_id, event_id) WHERE deleted_at IS NULL に
	// ぶつかる。先にごみ箱行を作って soft delete してから、生きている行を作る。
	trashedID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: trashedID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "trashed.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed trashed original: %v", err)
	}
	if _, err := q.SoftDeleteRecording(ctx, trashedID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	liveID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: liveID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "live.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed live original: %v", err)
	}

	ids, err := q.ListRecordingIDsMissingThumbnail(ctx)
	if err != nil {
		t.Fatalf("ListRecordingIDsMissingThumbnail: %v", err)
	}

	foundLive := false
	for _, id := range ids {
		if id == trashedID {
			t.Errorf("trashed recording %d must not be in missing-thumbnail list, got %v", trashedID, ids)
		}
		if id == liveID {
			foundLive = true
		}
	}
	if !foundLive {
		t.Errorf("live recording %d must be in missing-thumbnail list, got %v", liveID, ids)
	}
}

// TestEnqueueMissingThumbnails_ExcludesTrash は EnqueueMissingThumbnails（復旧・
// テスト用の全件投入）でも、ごみ箱の録画にはジョブを積まないことを固定する。
func TestEnqueueMissingThumbnails_ExcludesTrash(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	workers := river.NewWorkers()
	river.AddWorker(workers, &ThumbnailWorker{})
	client, err := NewClient(pool, workers, ClientConfig{PeriodicJobs: false})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	trashedID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: trashedID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "trashed2.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed trashed original: %v", err)
	}
	if _, err := q.SoftDeleteRecording(ctx, trashedID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	n, err := EnqueueMissingThumbnails(ctx, pool, client)
	if err != nil {
		t.Fatalf("EnqueueMissingThumbnails: %v", err)
	}
	if n != 0 {
		t.Errorf("EnqueueMissingThumbnails enqueued %d jobs, want 0 (only the trashed recording is missing a thumbnail)", n)
	}
}

// fakeThumbnailTools は ffprobe が durationSec を返し、ffmpeg が tinyJPEG を
// 出力パスに書くフックを返す。
func fakeThumbnailTools(t *testing.T, durationSec float64) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// ffprobe: duration 行を返す。
		if strings.Contains(name, "ffprobe") || containsArg(args, "format=duration") {
			return []byte(fmt.Sprintf("%g\n", durationSec)), nil
		}
		// ffmpeg: 最後の引数が出力パス。
		if len(args) == 0 {
			return nil, fmt.Errorf("ffmpeg: no args")
		}
		out := args[len(args)-1]
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(out, tinyJPEG, 0o644); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// TestExtractFrameBakesSAR は extractFrame が SAR（ピクセル縦横比）を正方形
// ピクセルへ焼き込む scale フィルタを渡すことを保証する。JPEG は SAR を運ばない
// ため、これが無いと anamorphic な地デジ（1440x1080 SAR 4:3 → DAR 16:9）が
// ブラウザで横に潰れて見える。解像度はハードコードせず SAR だけで正規化する
// （BS の 1920x1080 SAR 1:1 は no-op）ので、フィルタ式そのものを固定する。
func TestExtractFrameBakesSAR(t *testing.T) {
	var gotArgs []string
	w := &ThumbnailWorker{
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return nil, nil
		},
	}
	if err := w.extractFrame(context.Background(), "in.ts", "out.jpg", 3*time.Second); err != nil {
		t.Fatalf("extractFrame: %v", err)
	}
	i := indexOfArg(gotArgs, "-vf")
	if i < 0 || i+1 >= len(gotArgs) {
		t.Fatalf("no -vf filter in args: %v", gotArgs)
	}
	const want = "scale=round(iw*sar/2)*2:ih,setsar=1"
	if got := gotArgs[i+1]; got != want {
		t.Errorf("-vf = %q, want %q", got, want)
	}
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func containsArg(args []string, substr string) bool {
	for _, a := range args {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
