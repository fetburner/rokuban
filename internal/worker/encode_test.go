package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/streamer"
	"github.com/fetburner/rokuban/internal/webhook"
)

func TestBuildFFmpegArgs(t *testing.T) {
	crf := 23
	p := config.EncodeProfile{
		Name:       "h264",
		Container:  "mp4",
		VideoCodec: "libx264",
		AudioCodec: "aac",
		Height:     720,
		CRF:        &crf,
		Preset:     "medium",
		ExtraArgs:  []string{"-movflags", "+faststart"},
	}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4", false, nil)

	// 必須フラグと入出力の位置。
	if !slices.Contains(args, "-i") {
		t.Fatal("missing -i")
	}
	if got := args[slices.Index(args, "-i")+1]; got != "/in.m2ts" {
		t.Errorf("input = %q, want /in.m2ts", got)
	}
	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("last arg = %q, want output path", args[len(args)-1])
	}
	if !slices.Contains(args, "-progress") {
		t.Fatal("missing -progress (must use pipe:1, not stderr scraping)")
	}
	if got := args[slices.Index(args, "-progress")+1]; got != "pipe:1" {
		t.Errorf("-progress target = %q, want pipe:1", got)
	}
	if !slices.Contains(args, "libx264") || !slices.Contains(args, "aac") {
		t.Errorf("args missing codecs: %v", args)
	}
	if !slices.Contains(args, "scale=-2:720") {
		t.Errorf("args missing scale filter: %v", args)
	}
	if !slices.Contains(args, "23") || !slices.Contains(args, "medium") {
		t.Errorf("args missing crf/preset: %v", args)
	}
	if !slices.Contains(args, "+faststart") {
		t.Errorf("args missing extra_args: %v", args)
	}
	// extra_args は -f（コンテナ）の前に置く（issue #321: 旧位置は -f の後ろ
	// だった。VOD と live の規則を 1 つにするための移動）。
	fIdx := -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" && args[i+1] == "mp4" {
			fIdx = i
			break
		}
	}
	extraIdx := slices.Index(args, "-movflags")
	if fIdx < 0 || extraIdx < 0 {
		t.Fatalf("missing -f mp4 / -movflags: %v", args)
	}
	if extraIdx >= fIdx {
		t.Errorf("extra_args (-movflags at %d) must come before -f (at %d): %v", extraIdx, fIdx, args)
	}
	// 自由形式のシェルは組み立てない。
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "&&") || strings.Contains(joined, "|") {
		t.Errorf("args look like shell: %v", args)
	}
}

// TestBuildFFmpegArgs_Deinterlace は VOD の実際の -vf 組み立て経路で、解除が縮小
// より前に入り、-vf が 1 本だけ出ることを固定する。
// 壊し方: BuildFFmpegArgs で profile.Deinterlace を渡さずに ScaleArgs を呼ぶ。
func TestBuildFFmpegArgs_Deinterlace(t *testing.T) {
	profile := config.EncodeProfile{
		Name:        "h264",
		Container:   "mp4",
		VideoCodec:  "libx264",
		AudioCodec:  "aac",
		Height:      720,
		Deinterlace: true,
	}
	args := BuildFFmpegArgs(profile, "/in.m2ts", "/out.mp4", false, nil)

	filterIdx := slices.Index(args, "-vf")
	if filterIdx < 0 || filterIdx+1 >= len(args) {
		t.Fatalf("missing -vf and filter value: %v", args)
	}
	if got := args[filterIdx+1]; got != "yadif,scale=-2:720" {
		t.Errorf("filter = %q, want %q", got, "yadif,scale=-2:720")
	}
	count := 0
	for _, arg := range args {
		if arg == "-vf" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("-vf count = %d, want 1: %v", count, args)
	}
}

func TestBuildFFmpegArgs_WebVTTSubtitleSidecar(t *testing.T) {
	p := config.EncodeProfile{
		Name: "web", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac", Subtitles: "webvtt",
	}
	args := BuildFFmpegArgs(p, "/in.ts", "/out.mp4", true, nil)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-map 0:s?", "-c:s webvtt", "-f webvtt", "/out.vtt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("subtitle args missing %q: %v", want, args)
		}
	}
}

// TestBuildFFmpegArgs_HWAccelBeforeInput は hwaccel ブロックが -i より前に来る
// ことを固定する（issue #321）。壊し方: 前置ブロックの append を -i の対の後ろへ移す。
func TestBuildFFmpegArgs_HWAccelBeforeInput(t *testing.T) {
	p := config.EncodeProfile{
		Name:       "h264_vaapi",
		Container:  "mp4",
		VideoCodec: "h264_vaapi",
		AudioCodec: "aac",
		Height:     720,
		Scaler:     ffargs.ScalerVAAPI,
		HWAccel: &ffargs.HWAccel{
			Kind:         "vaapi",
			Device:       "/dev/dri/renderD128",
			OutputFormat: "vaapi",
		},
	}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4", false, nil)

	hwIdx := slices.Index(args, "-hwaccel")
	deviceIdx := slices.Index(args, "-hwaccel_device")
	outputFormatIdx := slices.Index(args, "-hwaccel_output_format")
	iIdx := slices.Index(args, "-i")
	if hwIdx < 0 || deviceIdx < 0 || outputFormatIdx < 0 || iIdx < 0 {
		t.Fatalf("missing -hwaccel/-hwaccel_device/-hwaccel_output_format/-i: %v", args)
	}
	if hwIdx >= iIdx || deviceIdx >= iIdx || outputFormatIdx >= iIdx {
		t.Errorf("hwaccel block must come before -i: hwaccel=%d device=%d output_format=%d i=%d: %v",
			hwIdx, deviceIdx, outputFormatIdx, iIdx, args)
	}
	if args[hwIdx+1] != "vaapi" {
		t.Errorf("-hwaccel value = %q, want vaapi", args[hwIdx+1])
	}
	if args[deviceIdx+1] != "/dev/dri/renderD128" {
		t.Errorf("-hwaccel_device value = %q, want /dev/dri/renderD128", args[deviceIdx+1])
	}
	if args[outputFormatIdx+1] != "vaapi" {
		t.Errorf("-hwaccel_output_format value = %q, want vaapi", args[outputFormatIdx+1])
	}
	if !slices.Contains(args, "scale_vaapi=w=-2:h=720") {
		t.Errorf("missing hw scale filter: %v", args)
	}
	if slices.Contains(args, "scale=-2:720") {
		t.Errorf("must not also emit the software scale filter: %v", args)
	}
	vfCount := 0
	for _, arg := range args {
		if arg == "-vf" {
			vfCount++
		}
	}
	if vfCount != 1 {
		t.Errorf("-vf count = %d, want 1: %v", vfCount, args)
	}
}

// TestBuildFFmpegArgs_QP は qp が -qp として出て -crf は出ないことを固定する。
// 壊し方: emit するフラグ名を -crf にする。
func TestBuildFFmpegArgs_QP(t *testing.T) {
	qp := 24
	p := config.EncodeProfile{
		Name:       "h264_vaapi",
		Container:  "mp4",
		VideoCodec: "h264_vaapi",
		AudioCodec: "aac",
		QP:         &qp,
	}
	args := BuildFFmpegArgs(p, "in", "out", false, nil)
	if slices.Contains(args, "-crf") {
		t.Errorf("-crf must not be emitted when only qp is set: %v", args)
	}
	qpIdx := slices.Index(args, "-qp")
	if qpIdx < 0 {
		t.Fatalf("missing -qp: %v", args)
	}
	if args[qpIdx+1] != "24" {
		t.Errorf("-qp value = %q, want 24", args[qpIdx+1])
	}
}

// TestBuildFFmpegArgs_InputAndOutputExtraArgsPositions は input_extra_args が
// -i より前、extra_args が -c:v より後かつ -f/-progress より前に来ることを固定する
// （issue #321）。壊し方: 2 つの append 先を入れ替える（罠そのものの回帰）。
func TestBuildFFmpegArgs_InputAndOutputExtraArgsPositions(t *testing.T) {
	p := config.EncodeProfile{
		Name:           "h264",
		Container:      "mp4",
		VideoCodec:     "libx264",
		AudioCodec:     "aac",
		InputExtraArgs: []string{"-re"},
		ExtraArgs:      []string{"-movflags", "+faststart"},
	}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4", false, nil)

	reIdx := slices.Index(args, "-re")
	iIdx := slices.Index(args, "-i")
	cvIdx := slices.Index(args, "-c:v")
	movflagsIdx := slices.Index(args, "-movflags")
	fIdx := -1
	progressIdx := slices.Index(args, "-progress")
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" && args[i+1] == "mp4" {
			fIdx = i
			break
		}
	}
	if reIdx < 0 || iIdx < 0 || cvIdx < 0 || movflagsIdx < 0 || fIdx < 0 || progressIdx < 0 {
		t.Fatalf("missing one of -re/-i/-c:v/-movflags/-f/-progress: %v", args)
	}
	if reIdx >= iIdx {
		t.Errorf("input_extra_args (-re at %d) must come before -i (at %d): %v", reIdx, iIdx, args)
	}
	if movflagsIdx <= cvIdx {
		t.Errorf("extra_args (-movflags at %d) must come after -c:v (at %d): %v", movflagsIdx, cvIdx, args)
	}
	if movflagsIdx >= fIdx || movflagsIdx >= progressIdx {
		t.Errorf("extra_args (-movflags at %d) must come before -f (at %d) and -progress (at %d): %v",
			movflagsIdx, fIdx, progressIdx, args)
	}
}

// TestBuildFFmpegArgs_AppOwnedTail は、ユーザーが渡せる引数を最大に積んでも
// アプリ所有の末尾（-f/-progress pipe:1/-loglevel error/出力パス）が動かないことを
// 固定する。壊し方: ユーザー引数を末尾の後ろに append する。
func TestBuildFFmpegArgs_AppOwnedTail(t *testing.T) {
	p := config.EncodeProfile{
		Name:           "h264",
		Container:      "mp4",
		VideoCodec:     "libx264",
		AudioCodec:     "aac",
		InputExtraArgs: []string{"-re"},
		ExtraArgs:      []string{"-movflags", "+faststart", "-an"},
	}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4", false, nil)

	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("last arg = %q, want output path", args[len(args)-1])
	}
	if got := args[len(args)-3]; got != "-loglevel" {
		t.Errorf("args[-3] = %q, want -loglevel", got)
	}
	progressCount := 0
	yCount := 0
	for i, a := range args {
		if a == "-progress" {
			progressCount++
			if i+1 >= len(args) || args[i+1] != "pipe:1" {
				t.Errorf("-progress target = %v, want pipe:1", args)
			}
		}
		if a == "-y" {
			yCount++
		}
	}
	if progressCount != 1 {
		t.Errorf("-progress count = %d, want 1: %v", progressCount, args)
	}
	if yCount != 1 {
		t.Errorf("-y count = %d, want 1: %v", yCount, args)
	}
}

func TestBuildFFmpegArgs_NoOptional(t *testing.T) {
	p := config.EncodeProfile{
		Name:       "copyish",
		Container:  "mkv",
		VideoCodec: "libx265",
		AudioCodec: "copy",
	}
	args := BuildFFmpegArgs(p, "in", "out", false, nil)
	if slices.Contains(args, "-crf") {
		t.Error("crf should be omitted when nil")
	}
	if slices.Contains(args, "-preset") {
		t.Error("preset should be omitted when empty")
	}
	if slices.Contains(args, "-vf") {
		t.Error("scale should be omitted when height=0")
	}
	if !slices.Contains(args, "mkv") {
		t.Errorf("container -f missing: %v", args)
	}
}

func TestEncodedRelPath(t *testing.T) {
	got, err := EncodedRelPath("20240101/120000_title_1024.m2ts", "h264", "mp4", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "20240101/120000_title_1024_h264.mp4"
	if got != want {
		t.Errorf("EncodedRelPath = %q, want %q", got, want)
	}

	// issue #186 (M4-14) 受け入れ「前置された原本から生成した派生物の rel_path が
	// sites/{site}/ を引き継ぐ」を固定する。EncodedRelPath 自身には変更を入れて
	// いない（pathDirSlash/pathBaseSlash が最後の "/" だけで dir/base を切るので、
	// dir が何階層でも同じロジックで前置が引き継がれる）。
	got, err = EncodedRelPath("sites/tokyo/20240101/120000_title_1024.m2ts", "h264", "mp4", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	want = "sites/tokyo/20240101/120000_title_1024_h264.mp4"
	if got != want {
		t.Errorf("EncodedRelPath (site-prefixed original) = %q, want %q", got, want)
	}

	// プロファイル名の ".." や "/" はパスに持ち込まない。サニタイズせずにエラーにする
	// （config.ValidateEncodeProfileName と同じ規則）。
	if got, err = EncodedRelPath("a.m2ts", "foo/../bar", "mkv", 0, false); err == nil {
		t.Errorf("EncodedRelPath with a profile name containing / and .. = %q, want an error", got)
	}
}

func TestParseFFmpegProgress_OutTimeMSIsMicroseconds(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	parseFFmpegProgress(strings.NewReader("out_time_ms=1000000\nprogress=end\n"), log, nil)

	if !strings.Contains(logs.String(), "out_time_ms=1000 progress=end") {
		t.Fatalf("out_time_ms must convert ffmpeg microseconds to milliseconds, log=%q", logs.String())
	}
}

func TestParseFFmpegProgress_ReportsOutTime(t *testing.T) {
	var got []time.Duration
	parseFFmpegProgress(
		strings.NewReader("out_time_ms=1000000\nprogress=continue\nout_time_us=2500000\nprogress=end\n"),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		func(outTime time.Duration) { got = append(got, outTime) },
	)

	want := []time.Duration{time.Second, 2500 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Fatalf("reported out times = %v, want %v", got, want)
	}
}

// installFakeFFmpeg は PATH 先頭に「入力を出力へコピーし progress を stdout に書く」
// 偽 ffmpeg を置き、元の PATH を復元する cleanup を返す。
func installFakeFFmpeg(t *testing.T) (ffmpegPath string) {
	t.Helper()
	dir := t.TempDir()
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.bat"
	}
	path := filepath.Join(dir, name)

	// 最後の引数が出力。入力は -i の次。
	script := `#!/bin/sh
set -e
input=""
output=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-i" ]; then input="$a"; fi
  prev="$a"
  output="$a"
done
if [ -z "$input" ] || [ -z "$output" ]; then
  echo "fake-ffmpeg: missing input/output" >&2
  exit 2
fi
# 進捗を pipe:1（stdout）へ。stderr には出さない。
printf 'out_time_ms=1000\nprogress=continue\n'
printf 'out_time_ms=2000\nprogress=end\n'
cp "$input" "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	return path
}

func seedEncodePolicy(t *testing.T, pool *pgxpool.Pool, recordingID int64, profiles []string) {
	t.Helper()
	if len(profiles) == 0 {
		return
	}
	// recording_encode_policy 衛星表（issue #159）。keep_original は
	// 定期 reconcile 候補 query が active encoded を完了扱いにするので
	// 'always' で十分 --- desired プロファイルの有無だけがテストの関心。
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
		 VALUES ($1, 'always', $2)`, recordingID, profiles); err != nil {
		t.Fatalf("seeding recording_encode_policy: %v", err)
	}
}

func writeEncodeOriginal(t *testing.T, mediaDir, relPath string, content []byte) {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func prepareEncodeInput(t *testing.T, pool *pgxpool.Pool, recordingID int64, mediaDir, relPath string, profiles []string, content []byte) int64 {
	t.Helper()
	seedEncodePolicy(t, pool, recordingID, profiles)
	writeEncodeOriginal(t, mediaDir, relPath, content)
	dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = relPath
		params.SizeBytes = int64(len(content))
	})
	return recordingID
}

// 同じ recording/profile の stale scratch は消し、別 profile、別 recording、別 kind は残す。
func TestEncodeWorker_RemovesStaleScratchForRecordingProfile(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	content := []byte("payload-0123456789")
	recordingID := dbtest.Recording(t, pool)
	seedEncodePolicy(t, pool, recordingID, []string{"h264"})
	writeEncodeOriginal(t, mediaDir, "20240101/stale.m2ts", content)
	dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = "20240101/stale.m2ts"
		params.SizeBytes = int64(len(content))
	})
	mk := func(rel string) string {
		dir := filepath.Join(scratchDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	stale := mk(filepath.Join("encode", encodeScratchScope(recordingID, "h264")+"-3-abc"))
	otherProfile := mk(filepath.Join("encode", encodeScratchScope(recordingID, "cut")+"-1-abc"))
	otherRecording := mk(filepath.Join("encode", encodeScratchScope(recordingID+1, "h264")+"-1-abc"))
	thumbnail := mk(fmt.Sprintf("thumbnail/%d-abc", recordingID))

	w := &EncodeWorker{
		Publisher:       pgstoretest.Media(pool),
		Progress:        pgstoretest.Media(pool),
		Attempts:        pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		ScratchDir:      scratchDir,
		FFmpeg:          ffmpegPath,
		Profiles: config.EncodeConfig{FFmpeg: ffmpegPath, Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
	}
	job := EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale scratch of the same recording/profile still exists (stat err = %v)", err)
	}
	for _, keep := range []string{otherProfile, otherRecording, thumbnail} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s must be kept: %v", keep, err)
		}
	}
}

func TestEncodeWorker_SuccessAndIdempotent(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/show.m2ts"
	content := []byte("fake-ts-payload-for-encode-test-0123456789")
	recordingID := dbtest.Recording(t, pool)
	seedEncodePolicy(t, pool, recordingID, []string{"h264"})
	writeEncodeOriginal(t, mediaDir, rel, content)
	dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = rel
		params.SizeBytes = int64(len(content))
	})

	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Progress:  pgstoretest.Media(pool),
		Attempts:  pgstoretest.Media(pool),

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		ScratchDir:      scratchDir,
		FFmpeg:          ffmpegPath,
		Profiles: config.EncodeConfig{
			FFmpeg: ffmpegPath,
			Profiles: []config.EncodeProfile{{
				Name:       "h264",
				Container:  "mp4",
				VideoCodec: "libx264",
				AudioCodec: "aac",
			}},
		},
	}

	job := EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}

	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() first run: %v", err)
	}

	encRel := "20240101/show_h264.mp4"
	outPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading encoded file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("encoded content = %q, want copy of input", got)
	}

	// media_assets 行が 1 件。
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		 WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'active'`,
		recordingID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("encoded assets = %d, want 1", count)
	}

	// 2 回目は冪等（再エンコードせず成功）。
	// ファイルを触って「上書きされない」ことを見る。
	marker := []byte("already-committed-should-not-overwrite")
	if err := os.WriteFile(outPath, marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() second run: %v", err)
	}
	got2, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(marker) {
		t.Errorf("second run rewrote file; idempotent skip failed")
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded'`,
		recordingID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("after second run encoded assets = %d, want 1", count)
	}

	// scratch が掃除されていること。
	entries, _ := os.ReadDir(scratchDir)
	if len(entries) != 0 {
		// encode/ 配下も空なら OK。親に encode が残っていても中身が空なら良い。
		for _, e := range entries {
			sub := filepath.Join(scratchDir, e.Name())
			if infos, err := os.ReadDir(sub); err == nil && len(infos) > 0 {
				t.Errorf("scratch not cleaned: %s still has %d entries", sub, len(infos))
			}
		}
	}
}

// 成功時に encode.finished が発火し、ペイロードに profile が載ること。
func TestEncodeWorker_FiresEncodeFinishedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/finished.m2ts"
	content := []byte("payload-for-webhook-finished-test")
	recordingID := dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
		params.Title = "encode test"
	})
	seedEncodePolicy(t, pool, recordingID, []string{"h264"})
	writeEncodeOriginal(t, mediaDir, rel, content)
	dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = rel
		params.SizeBytes = int64(len(content))
	})

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Progress:  pgstoretest.Media(pool),
		Attempts:  pgstoretest.Media(pool),

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		ScratchDir:      scratchDir,
		FFmpeg:          ffmpegPath,
		Profiles: config.EncodeConfig{
			FFmpeg: ffmpegPath,
			Profiles: []config.EncodeProfile{{
				Name:       "h264",
				Container:  "mp4",
				VideoCodec: "libx264",
				AudioCodec: "aac",
			}},
		},
		Webhook: client,
	}

	job := EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work: %v", err)
	}

	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.Type != webhook.EventEncodeFinished {
		t.Errorf("type = %q, want %q", got.Type, webhook.EventEncodeFinished)
	}
	if got.RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", got.RecordingID, recordingID)
	}
	if got.Profile != "h264" {
		t.Errorf("profile = %q, want h264", got.Profile)
	}
	if got.Status != "finished" {
		t.Errorf("status = %q, want finished", got.Status)
	}
	if got.Title != "encode test" {
		t.Errorf("title = %q, want %q", got.Title, "encode test")
	}
	// attempt は失敗イベント専用（成功は 1 回しか配送されない）。
	if got.Attempt != 0 || got.MaxAttempts != 0 {
		t.Errorf("attempt/maxAttempts = %d/%d, want 0/0 on finished", got.Attempt, got.MaxAttempts)
	}

	// 冪等スキップ（既に active な encoded がある）では再発火しない。
	rec.reset()
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() second run: %v", err)
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired on idempotent skip: %+v", events)
	}
}

// 失敗時に encode.failed が発火すること。ペイロードの profile はジョブ引数（設定に
// 存在しない名前でも）そのまま載り、River の試行回数も載る（恒久的な失敗では
// 試行ごとに配送されるので、受け側が最終試行を見分けられるようにする）。
func TestEncodeWorker_FiresEncodeFailedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	rel := "x/fail.m2ts"
	content := []byte("data")
	recordingID := dbtest.Recording(t, pool)
	writeEncodeOriginal(t, mediaDir, rel, content)
	dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = rel
		params.SizeBytes = int64(len(content))
	})

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Progress:  pgstoretest.Media(pool),
		Attempts:  pgstoretest.Media(pool),

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Profiles:        config.EncodeConfig{}, // "missing" プロファイルは未定義 → 失敗する
		Webhook:         client,
	}

	job := EncodeJobArgs{RecordingID: recordingID, Profile: "missing"}
	for i := int32(0); i < 2; i++ {
		start, err := w.beginEncodeAttempt(context.Background(), recordingID, "missing")
		if err != nil {
			t.Fatal(err)
		}
		updated, err := w.markEncodeAttemptFailed(context.Background(), recordingID, "missing", start.Count, errors.New("previous failure"))
		if err != nil || !updated {
			t.Fatalf("recording prior failure %d: updated=%v err=%v", i+1, updated, err)
		}
	}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err == nil {
		t.Fatal("expected error for unknown profile")
	}

	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.Type != webhook.EventEncodeFailed {
		t.Errorf("type = %q, want %q", got.Type, webhook.EventEncodeFailed)
	}
	if got.RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", got.RecordingID, recordingID)
	}
	if got.Profile != "missing" {
		t.Errorf("profile = %q, want missing", got.Profile)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Attempt != 3 || got.MaxAttempts != 25 {
		t.Errorf("attempt/maxAttempts = %d/%d, want 3/25", got.Attempt, got.MaxAttempts)
	}
}

// encode.failed を発火するかの判定（両方向）。Work 越しの ctx キャンセル
// テストは notify 内の DB 読みも同時に失敗するため、この分岐だけを分離して見る。
func TestShouldCountEncodeFailure(t *testing.T) {
	boom := errors.New("ffmpeg failed")
	cases := []struct {
		name   string
		err    error
		ctxErr error
		want   bool
	}{
		{"failure with live ctx", boom, nil, true},
		{"failure while ctx canceled", boom, context.Canceled, false},
		{"failure while ctx deadline exceeded", boom, context.DeadlineExceeded, true},
		{"success", nil, nil, false},
		{"success while ctx canceled", nil, context.Canceled, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCountEncodeFailure(c.err, c.ctxErr); got != c.want {
				t.Errorf("shouldCountEncodeFailure(%v, %v) = %v, want %v", c.err, c.ctxErr, got, c.want)
			}
		})
	}
}

// 停止による ctx キャンセルでは発火しないこと。判定そのものは
// TestShouldCountEncodeFailure が見る（ここは Work 越しに POST が飛ばないことの確認）。
func TestEncodeWorker_CtxCanceled_DoesNotFireWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	rel := "x/cancel.m2ts"
	content := []byte("data")
	recordingID := dbtest.Recording(t, pool)
	writeEncodeOriginal(t, mediaDir, rel, content)
	dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = rel
		params.SizeBytes = int64(len(content))
	})

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Progress:  pgstoretest.Media(pool),
		Attempts:  pgstoretest.Media(pool),

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
		Webhook: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	job := EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}
	if err := runWorkerViaJobQFake(ctx, w, job); err == nil {
		t.Fatal("expected error with canceled ctx")
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired on ctx cancel: %+v", events)
	}
}

func TestEncodeJobArgs_InsertOptsQueue(t *testing.T) {
	spec := EncodeJobArgs{}.Spec()
	if spec.Queue != "encode" {
		t.Errorf("Spec().Queue = %q, want encode", spec.Queue)
	}
	if !spec.Coalesce {
		t.Error("Spec().Coalesce should be true")
	}
	if spec.MaxAttempts != 26 {
		t.Errorf("Spec().MaxAttempts = %d, want 26 (greater than the domain limit 25)", spec.MaxAttempts)
	}
}

// TestBuildFFmpegArgs_SubtitleFixSubDuration は VOD 側でも ARIB 字幕の duration
// 欠如への対処が -i より前に入ることを固定する。
//
// 実測（自前ビルドの ffmpeg n7.1.1 + libaribcaption、NHK Eテレの実 TS 30 秒）:
// 無しでは 11/11 cue の終了時刻が約 1193 時間になり字幕が消えない。付けると 11/11 正常。
// 壊し方: append を消す / -i の後ろへ移す。
func TestBuildFFmpegArgs_SubtitleFixSubDuration(t *testing.T) {
	p := config.EncodeProfile{
		Name: "web", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac", Subtitles: "webvtt",
	}

	args := BuildFFmpegArgs(p, "/in.ts", "/out.mp4", true, nil)
	fixIdx, inputIdx := -1, -1
	for i, a := range args {
		switch a {
		case "-fix_sub_duration":
			fixIdx = i
		case "-i":
			if inputIdx < 0 {
				inputIdx = i
			}
		}
	}
	if fixIdx < 0 {
		t.Fatalf("-fix_sub_duration missing: %v", args)
	}
	if fixIdx > inputIdx {
		t.Errorf("-fix_sub_duration at %d must precede -i at %d: %v", fixIdx, inputIdx, args)
	}

	// ffprobe が字幕なしと判定した録画では付けない（VOD 側は heartbeat も使わない
	// --- セグメントが無いので分割する意味がない）。
	off := strings.Join(BuildFFmpegArgs(p, "/in.ts", "/out.mp4", false, nil), " ")
	if strings.Contains(off, "-fix_sub_duration") {
		t.Errorf("captionless args must not carry -fix_sub_duration: %s", off)
	}
	if strings.Contains(strings.Join(args, " "), "heartbeat") {
		t.Errorf("VOD args must not carry the live heartbeat flag: %v", args)
	}
}

func TestProbeHasSubtitlesRequestsSubtitleStreams(t *testing.T) {
	var gotName string
	var gotArgs []string
	hasSubtitles, err := probeHasSubtitles(context.Background(), "/tools/ffprobe", "/recording.ts",
		func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotName = name
			gotArgs = append([]string(nil), args...)
			return []byte("2\n"), nil
		})
	if err != nil {
		t.Fatalf("probeHasSubtitles: %v", err)
	}
	if !hasSubtitles {
		t.Fatal("probeHasSubtitles = false, want a subtitle stream")
	}
	if gotName != "/tools/ffprobe" {
		t.Errorf("ffprobe = %q, want /tools/ffprobe", gotName)
	}
	wantArgs := []string{
		"-v", "error", "-select_streams", "s",
		"-show_entries", "stream=index", "-of", "csv=p=0", "/recording.ts",
	}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Errorf("ffprobe args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestEncodeWorkerSubtitleProbeAndEncodeWithRealBinaries(t *testing.T) {
	ffmpeg, ffmpegErr := exec.LookPath("ffmpeg")
	ffprobe, ffprobeErr := exec.LookPath("ffprobe")
	if ffmpegErr != nil || ffprobeErr != nil {
		if os.Getenv("ROKUBAN_REQUIRE_FFMPEG") != "" {
			t.Fatalf("ffmpeg/ffprobe not in PATH but ROKUBAN_REQUIRE_FFMPEG is set: %v %v", ffmpegErr, ffprobeErr)
		}
		t.Skipf("ffmpeg/ffprobe not in PATH: %v %v", ffmpegErr, ffprobeErr)
	}

	dir := t.TempDir()
	subtitles := filepath.Join(dir, "captions.srt")
	srt := "1\n00:00:00,100 --> 00:00:01,500\ncaption fixture\n"
	if err := os.WriteFile(subtitles, []byte(srt), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.mkv")
	createArgs := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
		"-i", subtitles,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0",
		"-c:v", "mpeg2video", "-c:a", "mp2", "-c:s", "srt",
		"-f", "matroska", input,
	}
	if out, err := exec.Command(ffmpeg, createArgs...).CombinedOutput(); err != nil {
		t.Fatalf("creating subtitle fixture: %v: %s", err, out)
	}

	withSubtitles, err := probeHasSubtitles(context.Background(), ffprobe, input, commandOutput)
	if err != nil {
		t.Fatalf("probing subtitle fixture: %v", err)
	}
	if !withSubtitles {
		t.Fatal("real ffprobe did not find the fixture subtitle stream")
	}
	captionless := filepath.Join(dir, "captionless.mkv")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-i", input, "-map", "0", "-map", "-0:s", "-c", "copy", captionless).CombinedOutput(); err != nil {
		t.Fatalf("creating captionless fixture: %v: %s", err, out)
	}
	captionlessHasSubtitles, err := probeHasSubtitles(context.Background(), ffprobe, captionless, commandOutput)
	if err != nil {
		t.Fatalf("probing captionless fixture: %v", err)
	}
	if captionlessHasSubtitles {
		t.Fatal("real ffprobe found a subtitle stream in the captionless fixture")
	}

	output := filepath.Join(dir, "encoded.mkv")
	subtitleOut, err := mediapath.SubtitleSibling(output)
	if err != nil {
		t.Fatal(err)
	}
	profile := config.EncodeProfile{
		Name: "fixture", Container: "matroska", VideoCodec: "mpeg2video", AudioCodec: "mp2", Subtitles: "webvtt",
	}
	w := &EncodeWorker{FFmpeg: ffmpeg}
	if err := w.runEncodeCommand(context.Background(), encodeCommandInput{
		profile: profile, inputPath: input, scratchOut: output, subtitleOut: subtitleOut, withSubtitles: withSubtitles,
	}, func(time.Duration) {}, slog.Default()); err != nil {
		t.Fatalf("encoding subtitle fixture: %v", err)
	}
}

func TestDerivedFSStorePublishesRealEncodeAndThumbnail(t *testing.T) {
	ffmpeg, ffmpegErr := exec.LookPath("ffmpeg")
	ffprobe, ffprobeErr := exec.LookPath("ffprobe")
	if ffmpegErr != nil || ffprobeErr != nil {
		t.Skipf("ffmpeg/ffprobe not in PATH: %v %v", ffmpegErr, ffprobeErr)
	}
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	fixturePath := filepath.Join(t.TempDir(), "fixture.ts")
	createArgs := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x90:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=2",
		"-t", "2", "-c:v", "mpeg2video", "-c:a", "mp2", "-f", "mpegts", fixturePath,
	}
	if out, err := exec.Command(ffmpeg, createArgs...).CombinedOutput(); err != nil {
		t.Fatalf("creating real encode fixture: %v: %s", err, out)
	}
	original, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	const originalRelPath = "20261030/real-encode.ts"
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, originalRelPath, []string{"h264"}, original)
	encodeWorker := newH264Worker(pool, mediaDir, scratchDir, ffmpeg)
	encodeWorker.FFprobe = ffprobe
	encodeWorker.Profiles = config.EncodeConfig{FFmpeg: ffmpeg, Profiles: []config.EncodeProfile{{
		Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac", Preset: "ultrafast",
	}}}
	if err := runWorkerViaJobQFake(context.Background(), encodeWorker, EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}); err != nil {
		t.Fatalf("real encode Work: %v", err)
	}
	encoded, err := pgstoretest.Media(pool).GetEncodedMediaAssetForServing(context.Background(), recordingID, "h264")
	if err != nil {
		t.Fatalf("loading active encoded asset: %v", err)
	}
	objects := testFSObjectStore(mediaDir)
	encodedObject, err := objects.Head(context.Background(), encoded.RelPath)
	if err != nil {
		t.Fatalf("Head real encoded object %q: %v", encoded.RelPath, err)
	}
	if encodedObject.Size == 0 || encodedObject.Size != encoded.SizeBytes {
		t.Fatalf("encoded object size = %d, active asset size = %d; want matching non-empty output", encodedObject.Size, encoded.SizeBytes)
	}
	encodedBytes, err := os.ReadFile(filepath.Join(mediaDir, filepath.FromSlash(encoded.RelPath)))
	if err != nil {
		t.Fatalf("reading real encoded object: %v", err)
	}
	if len(encodedBytes) < 8 || string(encodedBytes[4:8]) != "ftyp" {
		t.Fatalf("real encoded object has no MP4 ftyp header: %x", encodedBytes[:min(len(encodedBytes), 12)])
	}

	thumbnailWorker := &ThumbnailWorker{
		Media: pgstoretest.Media(pool), Objects: objects, ObjectPublisher: pgstoretest.Media(pool),
		MediaDir: mediaDir, ScratchDir: t.TempDir(), FFmpeg: ffmpeg, FFprobe: ffprobe,
	}
	if err := runWorkerViaJobQFake(context.Background(), thumbnailWorker, ThumbnailJobArgs{RecordingID: recordingID}); err != nil {
		t.Fatalf("real thumbnail Work: %v", err)
	}
	thumbnail, err := pgstoretest.Media(pool).GetThumbnailMediaAssetForServing(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("loading active thumbnail asset: %v", err)
	}
	thumbnailObject, err := objects.Head(context.Background(), thumbnail.RelPath)
	if err != nil {
		t.Fatalf("Head real thumbnail object %q: %v", thumbnail.RelPath, err)
	}
	if thumbnailObject.Size == 0 || thumbnailObject.Size != thumbnail.SizeBytes {
		t.Fatalf("thumbnail object size = %d, active asset size = %d; want matching non-empty output", thumbnailObject.Size, thumbnail.SizeBytes)
	}
	thumbnailBytes, err := os.ReadFile(filepath.Join(mediaDir, filepath.FromSlash(thumbnail.RelPath)))
	if err != nil {
		t.Fatalf("reading real thumbnail object: %v", err)
	}
	if len(thumbnailBytes) < 4 || !bytes.Equal(thumbnailBytes[:2], []byte{0xff, 0xd8}) {
		t.Fatalf("real thumbnail is not a JPEG: %x", thumbnailBytes[:min(len(thumbnailBytes), 8)])
	}

	router := chi.NewRouter()
	streamer.New(pgstoretest.Media(pool), streamer.Config{
		MediaDir: mediaDir, ObjectStores: map[string]objstore.Store{dbtest.StoreName: objects},
	}).Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		fmt.Sprintf("%s/api/media/recordings/%d/file?profile=h264", srv.URL, recordingID), nil)
	if err != nil {
		t.Fatalf("building encoded range request: %v", err)
	}
	request.Header.Set("Range", "bytes=0-9")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("requesting a range from the real encoded object: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	rangeBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading encoded range response: %v", err)
	}
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("encoded Range status = %d, want 206", response.StatusCode)
	}
	if got, want := response.Header.Get("Content-Range"), fmt.Sprintf("bytes 0-9/%d", len(encodedBytes)); got != want {
		t.Errorf("encoded Content-Range = %q, want %q", got, want)
	}
	if !bytes.Equal(rangeBody, encodedBytes[:10]) {
		t.Errorf("encoded Range body = %x, want %x", rangeBody, encodedBytes[:10])
	}
}
