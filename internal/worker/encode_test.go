package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ffargs"
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
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4")

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
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4")

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
	args := BuildFFmpegArgs(p, "in", "out")
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
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4")

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
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4")

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
	args := BuildFFmpegArgs(p, "in", "out")
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
	got, err := EncodedRelPath("20240101/120000_title_1024.m2ts", "h264", "mp4")
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
	got, err = EncodedRelPath("sites/tokyo/20240101/120000_title_1024.m2ts", "h264", "mp4")
	if err != nil {
		t.Fatal(err)
	}
	want = "sites/tokyo/20240101/120000_title_1024_h264.mp4"
	if got != want {
		t.Errorf("EncodedRelPath (site-prefixed original) = %q, want %q", got, want)
	}

	// プロファイル名の ".." や "/" はパスに持ち込まない。
	got, err = EncodedRelPath("a.m2ts", "foo/../bar", "mkv")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "..") {
		t.Errorf("path contains ..: %q", got)
	}
	if strings.Count(got, "/") != 0 {
		t.Errorf("profile must not introduce hierarchy: %q", got)
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

func seedRecordingWithOriginal(t *testing.T, pool *pgxpool.Pool, mediaDir, relPath string, profiles []string, content []byte) int64 {
	t.Helper()
	q := sqlcgen.New(pool)
	id, err := q.CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           int32(time.Now().UnixNano() % 1_000_000_000), // 衝突回避
		ServiceName:       "テスト",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "encode test",
		ProgramStartAt:    time.Now(),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
	if len(profiles) > 0 {
		// recording_encode_policy 衛星表（issue #159）。keep_original は
		// EnqueueMissingEncodes（このファイルがテストする対象）が見ないので
		// 'always' で十分 --- desired プロファイルの有無だけがテストの関心。
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
			 VALUES ($1, 'always', $2)`, id, profiles); err != nil {
			t.Fatalf("seeding recording_encode_policy: %v", err)
		}
	}
	full, err := filepath.Abs(filepath.Join(mediaDir, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	profileNil := (*string)(nil)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindOriginal,
		Profile:     profileNil,
		RelPath:     relPath,
		SizeBytes:   int64(len(content)),
	}); err != nil {
		t.Fatalf("CreateMediaAsset original: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM media_assets WHERE recording_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM recordings WHERE id = $1`, id)
	})
	return id
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
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     ffmpegPath,
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

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}

	if err := w.Work(context.Background(), job); err != nil {
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
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'h264' AND state = 'active'`,
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
	if err := w.Work(context.Background(), job); err != nil {
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
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     ffmpegPath,
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

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}
	if err := w.Work(context.Background(), job); err != nil {
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
	if err := w.Work(context.Background(), job); err != nil {
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
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/fail.m2ts", nil, []byte("data"))

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Pool:     pool,
		MediaDir: mediaDir,
		Profiles: config.EncodeConfig{}, // "missing" プロファイルは未定義 → 失敗する
		Webhook:  client,
	}

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 3, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "missing"},
	}
	if err := w.Work(context.Background(), job); err == nil {
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
func TestShouldNotifyEncodeFailure(t *testing.T) {
	boom := errors.New("ffmpeg failed")
	cases := []struct {
		name   string
		err    error
		ctxErr error
		want   bool
	}{
		{"failure with live ctx", boom, nil, true},
		{"failure while ctx canceled", boom, context.Canceled, false},
		{"failure while ctx deadline exceeded", boom, context.DeadlineExceeded, false},
		{"success", nil, nil, false},
		{"success while ctx canceled", nil, context.Canceled, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldNotifyEncodeFailure(c.err, c.ctxErr); got != c.want {
				t.Errorf("shouldNotifyEncodeFailure(%v, %v) = %v, want %v", c.err, c.ctxErr, got, c.want)
			}
		})
	}
}

// ctx キャンセル（River の停止・タイムアウト）では発火しないこと。判定そのものは
// TestShouldNotifyEncodeFailure が見る（ここは Work 越しに POST が飛ばないことの確認）。
func TestEncodeWorker_CtxCanceled_DoesNotFireWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/cancel.m2ts", nil, []byte("data"))

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Pool:     pool,
		MediaDir: mediaDir,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
		Webhook: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}
	if err := w.Work(ctx, job); err == nil {
		t.Fatal("expected error with canceled ctx")
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired on ctx cancel: %+v", events)
	}
}

func TestEnqueueMissingEncodes_LevelTrigger(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatal(err)
	}

	mediaDir := t.TempDir()
	content := []byte("payload")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/a.m2ts",
		[]string{"h264", "h265"}, content)

	// h264 は既に encoded 済み → 投入しない。
	h264 := "h264"
	q := sqlcgen.New(pool)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "x/a_h264.mp4",
		SizeBytes:   1,
	}); err != nil {
		t.Fatal(err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := EnqueueMissingEncodes(context.Background(), client, pool, recordingID); err != nil {
		t.Fatalf("EnqueueMissingEncodes: %v", err)
	}

	// h265 だけ 1 件。
	var kinds []string
	rows, err := pool.Query(context.Background(),
		`SELECT args->>'profile' FROM river_job WHERE kind = 'encode' AND args->>'recording_id' = $1::text`,
		recordingID,
	)
	if err != nil {
		// recording_id は JSON number。文字列比較を避ける。
		rows, err = pool.Query(context.Background(),
			`SELECT args->>'profile' FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1`,
			recordingID,
		)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kinds, []string{"h265"}) {
		t.Errorf("enqueued profiles = %v, want [h265]", kinds)
	}

	// 再呼び出しは UniqueOpts で重複スキップ（エラーにならない）。
	if err := EnqueueMissingEncodes(context.Background(), client, pool, recordingID); err != nil {
		t.Fatalf("second EnqueueMissingEncodes: %v", err)
	}
}

func TestEncodeJobArgs_InsertOptsQueue(t *testing.T) {
	opts := EncodeJobArgs{}.InsertOpts()
	if opts.Queue != encodeQueue {
		t.Errorf("Queue = %q, want %q", opts.Queue, encodeQueue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs should be true")
	}
}

// EncodeEnqueueHintArgs（issue #133 の事後追加ヒントジョブ）は encode キューと
// pending 状態での ByArgs 一意化を使うこと。
func TestEncodeEnqueueHintArgs_InsertOptsQueue(t *testing.T) {
	opts := EncodeEnqueueHintArgs{}.InsertOpts()
	if opts.Queue != encodeQueue {
		t.Errorf("Queue = %q, want %q", opts.Queue, encodeQueue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs should be true")
	}
}

// river.ClientFromContextSafely が失敗する ctx（River の外から Work を直接呼んだ
// 場合）では、ruler_pass 完了時の reconcile_pass ヒントのように黙って何もしない
// のではなくエラーを返すこと。このジョブ自体の主目的が「encode ジョブを実際に
// 投入すること」であるため、client が無いことをサイレントな no-op にすると
// ユーザーの事後追加依頼が消えてしまう（EncodeEnqueueHintWorker.Work の doc
// コメント参照）。
func TestEncodeEnqueueHintWorker_Work_WithoutClient_Errors(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &EncodeEnqueueHintWorker{Pool: pool}
	job := &river.Job[EncodeEnqueueHintArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeEnqueueHintArgs{RecordingID: 999999},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("expected error when no river client is attached to ctx, got nil")
	}
}

// EncodeEnqueueHintWorker は EncodeEnqueueHintArgs ジョブを実際の River クライアント
// 経由で処理すると、EnqueueMissingEncodes を呼んで desired（recordings.encode_profiles）
// − observed（active encoded media_assets）の差分を encode ジョブとして投入すること。
// 既に active encoded な h264 は再投入せず h265 だけが投入されることまで見る（issue
// #133 の受け入れ「予約が無い録画で事後追加が成功し、encode_profiles に反映されて
// encode ジョブが投入されること」の worker 側の裏付け --- api 側は
// internal/api/recordings_encode_profiles_test.go が見る）。
func TestEncodeEnqueueHintWorker_EnqueuesMissingEncodes(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatal(err)
	}

	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "hint/a.m2ts",
		[]string{"h264", "h265"}, []byte("payload"))

	h264 := "h264"
	q := sqlcgen.New(pool)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "hint/a_h264.mp4",
		SizeBytes:   1,
	}); err != nil {
		t.Fatal(err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		cancel()
		<-client.Stopped()
	}()

	if _, err := client.Insert(context.Background(), EncodeEnqueueHintArgs{RecordingID: recordingID}, nil); err != nil {
		t.Fatalf("inserting encode_enqueue_hint job: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-subscribeCh:
			if event.Job.Kind == "encode_enqueue_hint" {
				goto hintCompleted
			}
		case <-deadline:
			t.Fatal("timed out waiting for encode_enqueue_hint completion")
		}
	}
hintCompleted:

	var profiles []string
	rows, err := pool.Query(context.Background(),
		`SELECT args->>'profile' FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1`,
		recordingID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("enqueued profiles = %v, want [h265] (h264 は既に active encoded なので投入されない)", profiles)
	}
}
