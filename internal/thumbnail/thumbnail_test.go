package thumbnail

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// tinyJPEG は最小限の有効な JPEG（SOI + EOI）。fake ffmpeg が書き出す。
var tinyJPEG = []byte{0xFF, 0xD8, 0xFF, 0xD9}

func TestThumbnailSeek(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestThumbnailRelPathUsesPersistedGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		generation int64
		want       string
	}{
		{0, "thumbnails/12.jpg"},
		{1, "thumbnails/12.g1.jpg"},
		{8, "thumbnails/12.g8.jpg"},
	} {
		if got := thumbnailRelPath(12, tc.generation); got != tc.want {
			t.Errorf("thumbnailRelPath(12, %d) = %q, want %q", tc.generation, got, tc.want)
		}
	}
}

func TestNextUnusedThumbnailRelPathAdvancesPastExistingFile(t *testing.T) {
	t.Parallel()
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, os.MkdirAll(filepath.Join(mediaDir, "thumbnails"), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(mediaDir, "thumbnails", "12.g3.jpg"), tinyJPEG, 0o644), "os.WriteFile")
	w := &ThumbnailWorker{
		Objects:  testFSObjectStore(mediaDir),
		MediaDir: mediaDir,
	}
	path, generation, err := w.nextUnusedThumbnailRelPath(context.Background(), 12, 3)
	testutil.RequireNoError(t, err, "nextUnusedThumbnailRelPath")
	if path != "thumbnails/12.g4.jpg" || generation != 4 {
		t.Fatalf("nextUnusedThumbnailRelPath = (%q, %d), want (%q, 4)", path, generation, "thumbnails/12.g4.jpg")
	}
}

func TestThumbnailNeedsReselect(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)

	// 原本ファイル + media_assets 行。
	origRel := "shows/ep1.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(origPath), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(origPath, []byte("fake-ts-content"), 0o644), "os.WriteFile")
	dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = origRel
		fixtureParams.SizeBytes = int64(len("fake-ts-content"))
	})

	w := &ThumbnailWorker{
		Objects:          testFSObjectStore(mediaDir),
		ObjectPublisher:  pgstoretest.Media(pool),
		Media:            pgstoretest.Media(pool),
		DerivedStoreName: dbtest.StoreName,
		MediaDir:         mediaDir,
		ScratchDir:       scratchDir,
		FFmpeg:           "ffmpeg",
		FFprobe:          "ffprobe",
		runCmd:           fakeThumbnailTools(t, 100.0),
	}

	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, job), "Work() error")

	// DB 行が 1 つ。
	state := requireThumbnailState(t, pool, recordingID, dbtest.StoreName)
	if state.SeekMs == nil || *state.SeekMs != 10000 {
		t.Errorf("initial thumbnail seek_ms = %v, want 10000", state.SeekMs)
	}
	assertMediaAssetCopy(t, pool, state.ID, state.RelPath)

	// メディア上に JPEG がある。
	dest := filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d.jpg", recordingID))
	data, err := os.ReadFile(dest)
	testutil.RequireNoError(t, err, "reading thumbnail file")
	if !bytesEqual(data, tinyJPEG) {
		t.Errorf("thumbnail content = %v, want tinyJPEG", data)
	}
	assertMediaAssetSHA256(t, pool, recordingID, store.AssetKindThumbnail, data)

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
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "reselect/original.ts", []byte("fake-ts"))
	oldRelPath := thumbnailRelPath(recordingID, 0)
	thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
		store.AssetKindThumbnail, nil, oldRelPath, []byte("old thumbnail generation"))
	setThumbnailSeek(t, pool, thumbnailID, 30000)
	seedCMRanges(t, pool, recordingID, []chapters.Range{{StartMs: 0, EndMs: 60000}})

	var gotInputSeek string
	w := &ThumbnailWorker{
		Objects:          testFSObjectStore(mediaDir),
		ObjectPublisher:  pgstoretest.Media(pool),
		Media:            pgstoretest.Media(pool),
		DerivedStoreName: dbtest.StoreName,
		MediaDir:         mediaDir,
		ScratchDir:       scratchDir,
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
	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work() error")
	// chapter boundary 60,000ms is quantized to source frame 59,993ms; the
	// 30s policy seek therefore maps to 89,993ms on the original timeline.
	if gotInputSeek != "89.993" {
		t.Errorf("ffmpeg -ss = %q, want 89.993 seconds", gotInputSeek)
	}

	state := requireThumbnailState(t, pool, recordingID, dbtest.StoreName)
	if state.ID != thumbnailID {
		t.Errorf("thumbnail row id = %d, want unchanged id %d", state.ID, thumbnailID)
	}
	if state.RelPath != fmt.Sprintf("thumbnails/%d.g1.jpg", recordingID) {
		t.Errorf("thumbnail rel_path = %q, want first replacement generation", state.RelPath)
	}
	if state.SeekMs == nil || *state.SeekMs != 89993 {
		t.Errorf("thumbnail seek_ms = %v, want 89993", state.SeekMs)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, filepath.FromSlash(oldRelPath))); err != nil {
		t.Errorf("old thumbnail path should remain through the transfer overlap (stat err = %v)", err)
	}
	newPath := filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d.g1.jpg", recordingID))
	if data, err := os.ReadFile(newPath); err != nil || !bytesEqual(data, tinyJPEG) {
		t.Errorf("new thumbnail file = %v, err = %v", data, err)
	}
	assertMediaAssetSHA256(t, pool, recordingID, store.AssetKindThumbnail, tinyJPEG)
	// 入力の原本は、差し替えの後も「active・dbtest.StoreName の置き場・実体無しの印なし」であること（active_present の条件）。
	orig, err := pgstoretest.Media(pool).GetActiveOriginalMediaAsset(ctx, recordingID)
	missing, missingErr := pgstoretest.Media(pool).ListAllMissingMediaAssetIDs(ctx)
	originalCopy := dbtest.MediaAssetCopyID(t, pool, originalID, dbtest.StoreName)
	marked := slices.Contains(missing, originalCopy)
	if err != nil || missingErr != nil || orig.ID != originalID || orig.Store != dbtest.StoreName || marked {
		t.Errorf("original input present after replacement: asset %d in %q (active err %v), missing marker %v (err %v), want asset %d in %q without a marker",
			orig.ID, orig.Store, err, marked, missingErr, originalID, dbtest.StoreName)
	}
}

func TestThumbnailWorker_UsesCutOnlyInputAndFrozenKeep(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	profile := "cut-profile"
	cutAssetID := seedMediaAsset(t, pool, mediaDir, recordingID,
		store.AssetKindEncoded, &profile, "encoded/cut.mp4", []byte("fake-mp4"))
	testutil.MustExec(t, pool, "seeding cut keep ranges",
		`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, $2::int8multirange)`,
		cutAssetID, "{[60000,960000)}")
	oldRelPath := thumbnailRelPath(recordingID, 0)
	thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
		store.AssetKindThumbnail, nil, oldRelPath, tinyJPEG)
	setThumbnailSeek(t, pool, thumbnailID, 30000)
	// 状態: active な原本が無い（検出の後で原本が削除された）。CM 検出は原本が実体を持つ時点で必要なので、先に作って検出してから削除する。
	seedDeletedOriginalCMRanges(t, pool, mediaDir, recordingID, []chapters.Range{{StartMs: 0, EndMs: 60000}})

	var gotInputSeek string
	w := &ThumbnailWorker{
		Objects:          testFSObjectStore(mediaDir),
		ObjectPublisher:  pgstoretest.Media(pool),
		Media:            pgstoretest.Media(pool),
		DerivedStoreName: dbtest.StoreName,
		MediaDir:         mediaDir,
		ScratchDir:       t.TempDir(),
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
	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work() error")
	if gotInputSeek != "30.000" {
		t.Errorf("cut input -ss = %q, want 30.000 seconds", gotInputSeek)
	}
	state := requireThumbnailState(t, pool, recordingID, dbtest.StoreName)
	if state.SeekMs == nil || *state.SeekMs != 90000 {
		t.Errorf("cut-only thumbnail seek_ms = %v, want 90000", state.SeekMs)
	}
	if state.ID != thumbnailID {
		t.Errorf("thumbnail id = %d, want unchanged id %d", state.ID, thumbnailID)
	}
}

func TestThumbnailWorker_UsesUncutEncodedInputWhenOriginalIsMissing(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	profile := "uncut-profile"
	encodedRelPath := "encoded/uncut.mp4"
	seedMediaAsset(t, pool, mediaDir, recordingID,
		store.AssetKindEncoded, &profile, encodedRelPath, []byte("fake-mp4"))
	thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
		store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), tinyJPEG)
	setThumbnailSeek(t, pool, thumbnailID, 30000)
	// 状態: 原本の行はあるが実体無しの印が付いている（原本は使えない入力）。
	seedLostOriginalCMRanges(t, pool, mediaDir, recordingID, []chapters.Range{{StartMs: 0, EndMs: 60000}})

	var gotInputSeek string
	var gotInput []byte
	w := &ThumbnailWorker{
		Objects:          testFSObjectStore(mediaDir),
		ObjectPublisher:  pgstoretest.Media(pool),
		Media:            pgstoretest.Media(pool),
		DerivedStoreName: dbtest.StoreName,
		MediaDir:         mediaDir,
		ScratchDir:       t.TempDir(),
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if i := indexOfArg(args, "-ss"); i >= 0 && i+1 < len(args) {
				gotInputSeek = args[i+1]
			}
			if i := indexOfArg(args, "-i"); i >= 0 && i+1 < len(args) {
				var err error
				gotInput, err = os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
			}
			if err := os.WriteFile(args[len(args)-1], tinyJPEG, 0o644); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work() error")
	if gotInputSeek != "89.993" {
		t.Errorf("uncut encoded input -ss = %q, want 89.993 seconds", gotInputSeek)
	}
	if !bytes.Equal(gotInput, []byte("fake-mp4")) {
		t.Errorf("ffmpeg input bytes = %q, want the uncut encoded object", gotInput)
	}
	state := requireThumbnailState(t, pool, recordingID, dbtest.StoreName)
	if state.SeekMs == nil || *state.SeekMs != 89993 {
		t.Errorf("thumbnail seek_ms = %v, want 89993", state.SeekMs)
	}
}

func TestThumbnailWorker_IdempotentRerun(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)

	origRel := "shows/ep2.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(origPath), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(origPath, []byte("ts"), 0o644), "os.WriteFile")
	dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = origRel
		fixtureParams.SizeBytes = 2
	})

	var ffmpegCalls int
	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		ScratchDir:      scratchDir,
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

	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, job), "first Work")
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, job), "second Work")

	// 2 回目は active thumbnail を見てスキップするので ffmpeg は 1 回だけ。
	if ffmpegCalls != 1 {
		t.Errorf("ffmpeg calls = %d, want 1 (second run must skip)", ffmpegCalls)
	}

	// 行は 1 つだけ。
	var n int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`,
		recordingID,
	).Scan(&n), "counting thumbnail media assets")
	if n != 1 {
		t.Errorf("thumbnail rows = %d, want 1", n)
	}
}

func TestThumbnailWorker_SkipsWithoutOriginal(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	recordingID := dbtest.Recording(t, pool)
	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(t.TempDir()),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool),
		MediaDir:        t.TempDir(),
		ScratchDir:      t.TempDir(),
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			t.Fatal("runCmd must not be called when original is missing")
			return nil, nil
		},
	}

	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, job), "Work() error")
}

func TestThumbnailWorker_RevivesTombstone(t *testing.T) {
	t.Parallel()
	// state='deleted' の tombstone（過去の完全削除の残骸）がある recording に
	// 対して Work を実行すると、行が active に戻り HasActiveThumbnailMediaAsset
	// が引けるようになる（issue #108）。
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)

	origRel := "shows/ep3.m2ts"
	origPath := filepath.Join(mediaDir, filepath.FromSlash(origRel))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(origPath), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(origPath, []byte("ts"), 0o644), "os.WriteFile")
	dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = origRel
		fixtureParams.SizeBytes = 2
	})

	// tombstone を作る: いったん active な thumbnail 行を作り、削除プロトコル
	// の最終状態（state='deleted', deleted_at 有り）に直接遷移させる。
	rel := thumbnailRelPath(recordingID, 0)
	dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.Kind = store.AssetKindThumbnail
		fixtureParams.RelPath = rel
	})
	dbtest.SetRecordingMediaAssetState(t, pool, recordingID, store.AssetKindThumbnail, "deleted")

	// tombstone がある状態では HasActiveThumbnailMediaAsset は false を返す。
	if has, err := pgstoretest.Media(pool).HasActiveThumbnailMediaAsset(context.Background(), recordingID); err != nil || has {
		t.Fatalf("precondition: active thumbnail present = %v, err = %v, want none", has, err)
	}

	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		ScratchDir:      scratchDir,
		runCmd:          fakeThumbnailTools(t, 100.0),
	}

	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, job), "Work() error")

	// tombstone が active に戻り、HasActiveThumbnailMediaAsset が true を返す。
	has, err := pgstoretest.Media(pool).HasActiveThumbnailMediaAsset(context.Background(), recordingID)
	testutil.RequireNoError(t, err, "checking revived thumbnail")
	if !has {
		t.Fatal("thumbnail asset not revived to active")
	}

	// 新規行を積まず、tombstone 行そのものを書き換えている（行は 1 つだけ）。
	var n int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail'`,
		recordingID,
	).Scan(&n), "counting thumbnail media assets")
	if n != 1 {
		t.Errorf("thumbnail rows = %d, want 1 (tombstone should be revived, not duplicated)", n)
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
	t.Parallel()
	var gotArgs []string
	w := &ThumbnailWorker{
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return nil, nil
		},
	}
	testutil.RequireNoError(t, w.extractFrame(context.Background(), "in.ts", "out.jpg", 3*time.Second), "extractFrame")
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

// 再選択の入口は候補クエリ 1 本なので、チャプター無しの録画は、後続 id に本物の候補があっても
// 作り直さない（`after = id-1, limit 1` が別録画の行を返す形）。候補の判定は SQL 側のテストが持つ。
func TestThumbnailWorker_ReselectSkipsNonCandidatesEvenWhenLaterRecordingIsCandidate(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()

	seed := func(name string, ranges []chapters.Range) (int64, string) {
		id := dbtest.Recording(t, pool)
		seedOriginalAsset(t, pool, mediaDir, id, name+"/original.ts", []byte("fake-ts"))
		rel := thumbnailRelPath(id, 0)
		thumbID := seedMediaAsset(t, pool, mediaDir, id, store.AssetKindThumbnail, nil, rel, tinyJPEG)
		setThumbnailSeek(t, pool, thumbID, 30000)
		if ranges != nil {
			seedCMRanges(t, pool, id, ranges)
		}
		return id, rel
	}
	noChapters, noChaptersRel := seed("nochap", nil)
	seed("candidate", []chapters.Range{{StartMs: 0, EndMs: 60000}})

	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		runCmd: func(context.Context, string, ...string) ([]byte, error) {
			t.Error("non-candidate recording must not run ffmpeg/ffprobe")
			return nil, fmt.Errorf("unexpected command")
		},
	}
	job := jobs.ThumbnailJobArgs{RecordingID: noChapters}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work() error")
	cur := requireThumbnailState(t, pool, noChapters, dbtest.StoreName)
	if cur.RelPath != noChaptersRel {
		t.Errorf("recording %d rel_path = %q, want unchanged %q", noChapters, cur.RelPath, noChaptersRel)
	}
}
