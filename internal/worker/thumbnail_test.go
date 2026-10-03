package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// tinyJPEG は最小限の有効な JPEG（SOI + EOI）。fake ffmpeg が書き出す。
var tinyJPEG = []byte{0xFF, 0xD8, 0xFF, 0xD9}

type countingJobInsertMiddleware struct {
	river.MiddlewareDefaults
	insertManyCalls int
	batchSizes      []int
	hasUniqueOpts   []bool
}

func (m *countingJobInsertMiddleware) InsertMany(ctx context.Context, params []*rivertype.JobInsertParams, doInner func(context.Context) ([]*rivertype.JobInsertResult, error)) ([]*rivertype.JobInsertResult, error) {
	m.insertManyCalls++
	m.batchSizes = append(m.batchSizes, len(params))
	for _, param := range params {
		m.hasUniqueOpts = append(m.hasUniqueOpts, len(param.UniqueKey) > 0 && param.UniqueStates != 0)
	}
	return doInner(ctx)
}

func newThumbnailInsertClient(t *testing.T, pool *pgxpool.Pool, middleware *countingJobInsertMiddleware) *river.Client[pgx5.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &ThumbnailWorker{})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:     map[string]river.QueueConfig{thumbnailQueue: {MaxWorkers: 1}},
		Workers:    workers,
		Middleware: []rivertype.Middleware{middleware},
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}
	return client
}

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

func TestThumbnailPlanForInputs(t *testing.T) {
	tests := []struct {
		name      string
		inputs    thumbnailInputs
		keep      []chapters.Range
		wantPath  string
		wantInput int64
		wantSeek  int64
		wantOK    bool
	}{
		{
			name: "original maps keep-axis seek back to the source",
			inputs: thumbnailInputs{Original: &thumbnailSource{
				MediaAssetID: 1, RelPath: "original.ts",
			}, Encoded: []thumbnailSource{{
				MediaAssetID: 2, Profile: "a", RelPath: "encoded.mp4",
			}}},
			keep:      []chapters.Range{{StartMs: 60000, EndMs: 1800000}},
			wantPath:  "original.ts",
			wantInput: 90000,
			wantSeek:  90000,
			wantOK:    true,
		},
		{
			name: "short first keep continues into the next keep range",
			inputs: thumbnailInputs{Original: &thumbnailSource{
				MediaAssetID: 1, RelPath: "original.ts",
			}},
			keep:      []chapters.Range{{StartMs: 0, EndMs: 20000}, {StartMs: 60000, EndMs: 360000}},
			wantPath:  "original.ts",
			wantInput: 70000,
			wantSeek:  70000,
			wantOK:    true,
		},
		{
			name: "encoded profiles are chosen lexically before cut profiles",
			inputs: thumbnailInputs{Encoded: []thumbnailSource{
				{MediaAssetID: 4, Profile: "z-un-cut", RelPath: "z.mp4"},
				{MediaAssetID: 3, Profile: "a-un-cut", RelPath: "a.mp4"},
				{MediaAssetID: 2, Profile: "0-cut", RelPath: "cut.mp4", Cut: true,
					KeepRanges: []chapters.Range{{StartMs: 0, EndMs: 500000}}},
			}},
			keep:      []chapters.Range{{StartMs: 60000, EndMs: 1800000}},
			wantPath:  "a.mp4",
			wantInput: 90000,
			wantSeek:  90000,
			wantOK:    true,
		},
		{
			name: "cut input uses its frozen keep duration and maps the fact back",
			inputs: thumbnailInputs{Encoded: []thumbnailSource{
				{MediaAssetID: 2, Profile: "cut", RelPath: "cut.mp4", Cut: true,
					KeepRanges: []chapters.Range{{StartMs: 60000, EndMs: 960000}}},
			}},
			keep:      []chapters.Range{{StartMs: 60000, EndMs: 1800000}},
			wantPath:  "cut.mp4",
			wantInput: 30000,
			wantSeek:  90000,
			wantOK:    true,
		},
		{
			name:   "empty keep has no chapter-aware plan",
			inputs: thumbnailInputs{Original: &thumbnailSource{MediaAssetID: 1, RelPath: "original.ts"}},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := thumbnailPlanForInputs(tt.inputs, tt.keep)
			if ok != tt.wantOK {
				t.Fatalf("plan available = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Source.RelPath != tt.wantPath || got.InputSeekMs != tt.wantInput || got.RecordedSeek != tt.wantSeek {
				t.Errorf("plan = {path:%q input:%d recorded:%d}, want {path:%q input:%d recorded:%d}",
					got.Source.RelPath, got.InputSeekMs, got.RecordedSeek,
					tt.wantPath, tt.wantInput, tt.wantSeek)
			}
		})
	}
}

func TestNextThumbnailRelPath(t *testing.T) {
	tests := []struct {
		previous string
		want     string
	}{
		{"thumbnails/12.jpg", "thumbnails/12.g1.jpg"},
		{"thumbnails/12.g1.jpg", "thumbnails/12.g2.jpg"},
		{"thumbnails/12.g8.jpg", "thumbnails/12.g9.jpg"},
	}
	for _, tt := range tests {
		if got := nextThumbnailRelPath(12, tt.previous); got != tt.want {
			t.Errorf("nextThumbnailRelPath(%q) = %q, want %q", tt.previous, got, tt.want)
		}
	}
}

func TestThumbnailNeedsReselect(t *testing.T) {
	inputs := thumbnailInputs{Original: &thumbnailSource{MediaAssetID: 1, RelPath: "original.ts"}}
	timeline := chapters.Derive(false, nil,
		[]chapters.Span{{StartMs: 0, EndMs: 60000, Label: chapters.LabelCM, Cut: true}}, 1800000)

	inside := int64(60000)
	if thumbnailNeedsReselect(&inside, inputs, timeline) {
		t.Fatal("a recorded frame at the first keep boundary should not be replaced")
	}
	inCM := int64(30000)
	if !thumbnailNeedsReselect(&inCM, inputs, timeline) {
		t.Fatal("a recorded frame in a CM range should be replaced")
	}
	if !thumbnailNeedsReselect(nil, inputs, timeline) {
		t.Fatal("an unknown legacy position should be replaced when a timeline and input exist")
	}
	if thumbnailNeedsReselect(&inCM, thumbnailInputs{}, timeline) {
		t.Fatal("no usable input should not enqueue a retry")
	}
	if thumbnailNeedsReselect(&inCM, inputs, nil) {
		t.Fatal("a recording without a chapter timeline should not be replaced")
	}

	allCut := chapters.Derive(true,
		[]chapters.Span{{StartMs: 0, EndMs: 1800000, Label: chapters.LabelCM, Cut: true}}, nil, 1800000)
	if thumbnailNeedsReselect(&inCM, inputs, allCut) {
		t.Fatal("an empty keep set should not repeatedly replace a thumbnail")
	}

	// A cut-only source can keep planning the exact same seek after a chapter edit.
	// The position is now in CM, but want == recorded must stop a repeat loop.
	cutOnly := thumbnailInputs{Encoded: []thumbnailSource{{
		MediaAssetID: 2, Profile: "cut", RelPath: "cut.mp4", Cut: true,
		KeepRanges: []chapters.Range{{StartMs: 0, EndMs: 500000}},
	}}}
	changed := chapters.Derive(true,
		[]chapters.Span{{StartMs: 30000, EndMs: 60000, Label: chapters.LabelCM, Cut: true}}, nil, 1800000)
	position := int64(30000)
	if thumbnailNeedsReselect(&position, cutOnly, changed) {
		t.Fatal("the same planned cut-only position should not be enqueued repeatedly")
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
	state, err := sqlcgen.New(pool).GetThumbnailPlanningState(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("loading thumbnail seek: %v", err)
	}
	if state.SeekMs == nil || *state.SeekMs != 10000 {
		t.Errorf("initial thumbnail seek_ms = %v, want 10000", state.SeekMs)
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

	// ジョブ固有 scratch directory は掃除されている。
	entries, err := os.ReadDir(filepath.Join(scratchDir, "thumbnail"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading thumbnail scratch dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch dirs left behind: %v", entries)
	}
}

func TestThumbnailWorker_ReplacesCMThumbnailWithNewGeneration(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "reselect/original.ts", []byte("fake-ts"))
	oldRelPath := thumbnailRelPath(recordingID)
	thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
		db.AssetKindThumbnail, nil, oldRelPath, tinyJPEG)
	q := sqlcgen.New(pool)
	if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thumbnailID,
		SeekMs:       30000,
	}); err != nil {
		t.Fatalf("seeding old thumbnail seek: %v", err)
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: recordingID,
		CmRanges:    "{[0,60000)}",
	}); err != nil {
		t.Fatalf("seeding CM ranges: %v", err)
	}

	var gotInputSeek string
	w := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if containsArg(args, "format=duration") {
				t.Fatal("chapter-aware reselection must not call ffprobe")
			}
			if i := indexOfArg(args, "-ss"); i >= 0 && i+1 < len(args) {
				gotInputSeek = args[i+1]
			}
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
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	// chapter boundary 60,000ms is quantized to source frame 59,993ms; the
	// 30s policy seek therefore maps to 89,993ms on the original timeline.
	if gotInputSeek != "89.993" {
		t.Errorf("ffmpeg -ss = %q, want 89.993 seconds", gotInputSeek)
	}

	state, err := q.GetThumbnailPlanningState(ctx, recordingID)
	if err != nil {
		t.Fatalf("loading thumbnail planning state: %v", err)
	}
	if state.ThumbnailMediaAssetID == nil || *state.ThumbnailMediaAssetID != thumbnailID {
		t.Errorf("thumbnail row id = %v, want unchanged id %d", state.ThumbnailMediaAssetID, thumbnailID)
	}
	if state.ThumbnailRelPath == nil || *state.ThumbnailRelPath != fmt.Sprintf("thumbnails/%d.g1.jpg", recordingID) {
		t.Errorf("thumbnail rel_path = %v, want first replacement generation", state.ThumbnailRelPath)
	}
	if state.SeekMs == nil || *state.SeekMs != 89993 {
		t.Errorf("thumbnail seek_ms = %v, want 89993", state.SeekMs)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, filepath.FromSlash(oldRelPath))); !os.IsNotExist(err) {
		t.Errorf("old thumbnail path still exists (stat err = %v)", err)
	}
	newPath := filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d.g1.jpg", recordingID))
	if data, err := os.ReadFile(newPath); err != nil || !bytesEqual(data, tinyJPEG) {
		t.Errorf("new thumbnail file = %v, err = %v", data, err)
	}
	if active, err := q.IsActiveThumbnailInput(ctx, originalID); err != nil || !active {
		t.Errorf("original input active after replacement = %v, err = %v", active, err)
	}
}

func TestThumbnailWorker_UsesCutOnlyInputAndFrozenKeep(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	profile := "cut-profile"
	cutAssetID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
		db.AssetKindEncoded, &profile, "encoded/cut.mp4", []byte("fake-mp4"))
	if _, err := pool.Exec(ctx,
		`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, $2::int8multirange)`,
		cutAssetID, "{[60000,960000)}"); err != nil {
		t.Fatalf("seeding cut keep ranges: %v", err)
	}
	oldRelPath := thumbnailRelPath(recordingID)
	thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
		db.AssetKindThumbnail, nil, oldRelPath, tinyJPEG)
	q := sqlcgen.New(pool)
	if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thumbnailID,
		SeekMs:       30000,
	}); err != nil {
		t.Fatalf("seeding old thumbnail seek: %v", err)
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: recordingID,
		CmRanges:    "{[0,60000)}",
	}); err != nil {
		t.Fatalf("seeding CM ranges: %v", err)
	}

	var gotInputSeek string
	w := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if i := indexOfArg(args, "-ss"); i >= 0 && i+1 < len(args) {
				gotInputSeek = args[i+1]
			}
			if err := os.WriteFile(args[len(args)-1], tinyJPEG, 0o644); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
	job := &river.Job[ThumbnailJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ThumbnailJobArgs{RecordingID: recordingID},
	}
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if gotInputSeek != "30.000" {
		t.Errorf("cut input -ss = %q, want 30.000 seconds", gotInputSeek)
	}
	state, err := q.GetThumbnailPlanningState(ctx, recordingID)
	if err != nil {
		t.Fatalf("loading thumbnail planning state: %v", err)
	}
	if state.SeekMs == nil || *state.SeekMs != 90000 {
		t.Errorf("cut-only thumbnail seek_ms = %v, want 90000", state.SeekMs)
	}
	if state.ThumbnailMediaAssetID == nil || *state.ThumbnailMediaAssetID != thumbnailID {
		t.Errorf("thumbnail id = %v, want unchanged id %d", state.ThumbnailMediaAssetID, thumbnailID)
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

func TestThumbnailUpsertDoesNotDuplicateExistingActiveRow(t *testing.T) {
	// ON CONFLICT DO UPDATE の UpsertThumbnailMediaAsset は、既に active な行がある
	// recording に対しても同じ行を更新する（行数は増えない）。
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
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

	// 同じ値で再度 upsert しても既存行の conflict にならず成功する。
	if _, err := sqlcgen.New(pool).UpsertThumbnailMediaAsset(context.Background(), sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID,
		RelPath:     rel,
		SizeBytes:   int64(len(tinyJPEG)),
	}); err != nil {
		t.Fatalf("UpsertThumbnailMediaAsset over existing active row: %v", err)
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
	// (site, network_id, service_id, event_id, program_start_at) WHERE deleted_at IS NULL に
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

func TestEnqueueMissingThumbnails_UsesInsertMany(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	liveIDs := []int64{insertTestRecording(t, pool), insertTestRecording(t, pool)}
	for i, id := range liveIDs {
		if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
			RecordingID: id,
			Kind:        db.AssetKindOriginal,
			RelPath:     fmt.Sprintf("live%d.m2ts", i),
			SizeBytes:   1,
		}); err != nil {
			t.Fatalf("seed live original %d: %v", id, err)
		}
	}

	// original が無い録画は対象外。
	insertTestRecording(t, pool)

	// active thumbnail がある録画は対象外。
	withThumbnailID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: withThumbnailID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "complete.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed complete original: %v", err)
	}
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: withThumbnailID,
		Kind:        db.AssetKindThumbnail,
		RelPath:     thumbnailRelPath(withThumbnailID),
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seed complete thumbnail: %v", err)
	}

	// ごみ箱の録画は対象外。
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

	middleware := &countingJobInsertMiddleware{}
	client := newThumbnailInsertClient(t, pool, middleware)
	n, err := EnqueueMissingThumbnails(ctx, pool, client)
	if err != nil {
		t.Fatalf("EnqueueMissingThumbnails: %v", err)
	}
	if n != len(liveIDs) {
		t.Fatalf("EnqueueMissingThumbnails returned %d, want %d", n, len(liveIDs))
	}
	if middleware.insertManyCalls != 1 {
		t.Fatalf("InsertMany calls = %d, want 1", middleware.insertManyCalls)
	}
	if len(middleware.batchSizes) != 1 || middleware.batchSizes[0] != len(liveIDs) {
		t.Fatalf("InsertMany batch sizes = %v, want [%d]", middleware.batchSizes, len(liveIDs))
	}
	for i, hasUniqueOpts := range middleware.hasUniqueOpts {
		if !hasUniqueOpts {
			t.Errorf("InsertMany param %d lost ThumbnailJobArgs unique options", i)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT (args->>'recording_id')::bigint, queue
		FROM river_job
		WHERE kind = 'thumbnail'
		ORDER BY id`)
	if err != nil {
		t.Fatalf("query thumbnail jobs: %v", err)
	}
	defer rows.Close()

	counts := make(map[int64]int, len(liveIDs))
	for rows.Next() {
		var id int64
		var queue string
		if err := rows.Scan(&id, &queue); err != nil {
			t.Fatalf("scan thumbnail job: %v", err)
		}
		if queue != thumbnailQueue {
			t.Errorf("thumbnail job %d queue = %q, want %q", id, queue, thumbnailQueue)
		}
		counts[id]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate thumbnail jobs: %v", err)
	}

	for _, id := range liveIDs {
		if counts[id] != 1 {
			t.Errorf("thumbnail jobs for live recording %d = %d, want 1", id, counts[id])
		}
	}
	for _, id := range []int64{withThumbnailID, trashedID} {
		if counts[id] != 0 {
			t.Errorf("thumbnail jobs for excluded recording %d = %d, want 0", id, counts[id])
		}
	}
}

func TestEnqueueMissingThumbnails_EmptySkipsRiver(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	// original が無い録画だけなら、River の投入 API 自体を呼ばない。
	insertTestRecording(t, pool)
	middleware := &countingJobInsertMiddleware{}
	client := newThumbnailInsertClient(t, pool, middleware)

	n, err := EnqueueMissingThumbnails(context.Background(), pool, client)
	if err != nil {
		t.Fatalf("EnqueueMissingThumbnails: %v", err)
	}
	if n != 0 {
		t.Fatalf("EnqueueMissingThumbnails returned %d, want 0", n)
	}
	if middleware.insertManyCalls != 0 {
		t.Fatalf("InsertMany calls = %d, want 0 for empty input", middleware.insertManyCalls)
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

func TestProbeDuration_IgnoresStderr(t *testing.T) {
	dir := t.TempDir()
	ffprobe := filepath.Join(dir, "ffprobe")
	const wantSeconds = "2546.360222"
	script := "#!/bin/sh\n" +
		"echo '[mpeg2video @ 0x1234] Invalid frame dimensions 0x0.' >&2\n" +
		"printf '%s\\n' '" + wantSeconds + "'\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := probeDuration(context.Background(), ffprobe, "input.m2ts", commandOutput)
	if err != nil {
		t.Fatalf("probeDuration() error: %v", err)
	}
	wantSec, err := strconv.ParseFloat(wantSeconds, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Duration(wantSec * float64(time.Second))
	if got != want {
		t.Errorf("probeDuration() = %v, want %v", got, want)
	}
}

// TestCommandOutput_IncludesStderrOnFailure は commandOutput が失敗時、
// *exec.ExitError の Stderr をエラーメッセージへ載せることを固定する
// （cmd.Output() は cmd.CombinedOutput() と違い stderr を戻り値に混ぜないため、
// 失敗時の診断はこの分岐でしか出てこない）。
func TestCommandOutput_IncludesStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-ffprobe")
	script := "#!/bin/sh\n" +
		"echo 'diagnostic: something went wrong' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := commandOutput(context.Background(), fake)
	if err == nil {
		t.Fatal("commandOutput() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "diagnostic: something went wrong") {
		t.Errorf("commandOutput() error = %q, want it to contain stderr diagnostic", err.Error())
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

// TestCommandOutput_WaitDelayExpiredOnSuccess_TreatedAsSuccess は、
// TestEncodeWorker_WaitDelayExpiredOnSuccess_TreatedAsSuccess（encode.go の
// runEncode）と双子の分岐である commandOutput 側の WaitDelay-on-success 扱いを
// 固定する。ThumbnailWorker.runCmd はテストフックとして commandOutput 自体を
// 迂回してしまうので、ここでは実 commandOutput を fd を漏らす偽 ffmpeg で直接
// 駆動する（installLeakyExitZeroFakeFFmpeg は encode_attempts_test.go と共有）。
//
// workerExecWaitDelay が実際に経過するのを待つ必要があるため数秒かかる。
func TestCommandOutput_WaitDelayExpiredOnSuccess_TreatedAsSuccess(t *testing.T) {
	sleepSeconds := int(workerExecWaitDelay/time.Second*3) + 5
	leakyFFmpeg, childPIDMarker := installLeakyExitZeroFakeFFmpeg(t, sleepSeconds)
	sleepStartedAt := time.Now()
	defer func() {
		if time.Since(sleepStartedAt) > time.Duration(sleepSeconds)*time.Second {
			return
		}
		pidBytes, err := os.ReadFile(childPIDMarker)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}()

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.ts")
	if err := os.WriteFile(inputPath, []byte("fake-input"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "output.bin")

	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	start := time.Now()
	out, err := commandOutput(context.Background(), leakyFFmpeg, "-i", inputPath, outputPath)
	if err != nil {
		t.Fatalf("commandOutput(): %v (log: %s)", err, logBuf.String())
	}
	// WaitDelay の経路を実際に踏んだことの裏付け --- 踏んでいなければ即座に
	// 返り、この分岐は何も検証していないことになる。
	if elapsed := time.Since(start); elapsed < workerExecWaitDelay/2 {
		t.Fatalf("commandOutput() returned after %s; want roughly workerExecWaitDelay (%s), the leaky child fd did not force a WaitDelay wait", elapsed, workerExecWaitDelay)
	}
	if !strings.Contains(logBuf.String(), "WaitDelay expired before I/O completed") {
		t.Errorf("log output = %q, want a warning distinguishing the WaitDelay-on-success path", logBuf.String())
	}
	// out が捨てられていないこと（installLeakyExitZeroFakeFFmpeg は progress
	// 行を標準出力へ書く。Output は stdout を返す）。
	if !strings.Contains(string(out), "progress=end") {
		t.Errorf("out = %q, want captured stdout to survive the WaitDelay-success path", out)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if string(got) != "fake-input" {
		t.Errorf("output file = %q, want copy of input", got)
	}
}
