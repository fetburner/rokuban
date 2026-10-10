package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// cutFFmpegProfile は実 ffmpeg の統合テストが使う最小の cut プロファイル。
// Scaler/Height を 0 にしてあるので、filtergraph は trim → concat → null だけに
// なり、測っているものが「切り取り」だけであることが保証される。
func cutFFmpegProfile() config.EncodeProfile {
	return config.EncodeProfile{
		Name:       "cut",
		Container:  "mp4",
		VideoCodec: "libx264",
		AudioCodec: "aac",
		Cut:        true,
	}
}

// TestCutEncode_RealFFmpeg_LengthAndAVSync は受け入れの「カット版の長さが keep
// 区間の合計と一致し、5 区間以上のカットで A/V のずれの累積が 1 フレーム以内に
// 収まる」を実 ffmpeg で測る。
//
// **音声を ms で切る実装に壊すとここが落ちる。** atrim の境界がフレーム番号から
// 換算されていないと、区間ごとに最大半フレームぶんの系統誤差が積み、5 区間で
// 1 フレームを超える（`ffargs.CutFilterComplex` の doc コメント参照）。
func TestCutEncode_RealFFmpeg_LengthAndAVSync(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	// 40 秒の素材。フレームレートは本番と同じ 30000/1001（31 秒ぶんのフレームが
	// 出ればよいので、testsrc の rate に同じ値を渡す）。
	gen := exec.CommandContext(context.Background(), ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30000/1001",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "40", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest",
		src,
	)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating source: %v (%s)", err, out)
	}

	// keep 区間はフレーム格子から作り、境界を**わざと非対称にずらす**:
	// start はフレームより手前、end はフレームより後ろ。生の ms で音声を切る
	// 実装ではこのずれが区間ごとに同じ向きへ積もる（下の atrim の説明を参照）。
	keep := adversarialKeepRanges(t)
	if len(keep) != 6 {
		t.Fatalf("expected 6 keep ranges, got %v", keep)
	}

	filter, err := ffargs.CutFilterComplex(keep, 0, 1, ffargs.ScalerSoftware, 0, false, false)
	if err != nil {
		t.Fatalf("CutFilterComplex: %v", err)
	}
	args := BuildFFmpegArgs(cutFFmpegProfile(), src, out, false, &filter)
	enc := exec.CommandContext(context.Background(), ffmpeg, args...)
	enc.Env = append(os.Environ(), "PATH="+filepath.Dir(ffmpeg)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if b, err := enc.CombinedOutput(); err != nil {
		t.Fatalf("cut encode failed: %v (%s)\nargs: %v", err, b, args)
	}

	// 期待する長さは filtergraph と同じ式（フレーム番号の合計）から出す。
	// ms の合計で期待すると、量子化のぶんだけテストの方がずれる。
	var frames int64
	for _, r := range keep {
		frames += chapters.MsToFrame(r.EndMs) - chapters.MsToFrame(r.StartMs)
	}
	frameSeconds := float64(chapters.FrameDenominator) / float64(chapters.FrameNumerator) // 30000/1001 の 1 フレーム
	wantSeconds := float64(frames) * frameSeconds

	videoDur, audioDur, formatDur := probeDurations(t, ffprobe, out)
	t.Logf("frames=%d want=%.3fs video=%.3fs audio=%.3fs format=%.3fs",
		frames, wantSeconds, videoDur, audioDur, formatDur)

	// 長さは 2 フレームまで許す。mp4 の duration は最終フレームの PTS + 1 フレーム
	// ぶんから数フレーム以内で丸まる（実測: want 29.997s に対して video 29.963s）。
	// 1 フレームに締めると、A/V のずれを測る下の判定と区別できない。
	tolerance := 2 * frameSeconds
	if diff := videoDur - wantSeconds; diff > tolerance || diff < -tolerance {
		t.Errorf("video duration %.3fs differs from the keep total %.3fs by more than two frames (%.3fs)",
			videoDur, wantSeconds, tolerance)
	}
	if diff := formatDur - wantSeconds; diff > tolerance || diff < -tolerance {
		t.Errorf("container duration %.3fs differs from the keep total %.3fs by more than two frames (%.3fs)",
			formatDur, wantSeconds, tolerance)
	}
	// A/V のずれ。atrim の境界がフレーム由来でないと、6 区間ぶんで 110ms
	// （≈3 フレーム）開く（実測: 生の ms で切ると audio が 0.11s ずれる）。
	if diff := videoDur - audioDur; diff > frameSeconds || diff < -frameSeconds {
		t.Errorf("audio drifted %.3fs from video, which is more than one frame (%.3fs)", diff, frameSeconds)
	}
}

// adversarialKeepRanges は「生の ms で音声を切る実装が必ず 1 フレームを超えて
// ずれる」境界を作る。
//
// 音声の合計と映像の合計の差は Σ[δ(end) − δ(start)]（δ = 生の ms − フレームの
// 時刻）なので、start を必ずフレームより手前（δ<0）・end を必ずフレームより後ろ
// （δ>0）に置くと、各区間が同じ向きに効いて差が積もる。**境界を対称にずらすと
// 打ち消し合って 0 になる**ので、この非対称が要る。
func adversarialKeepRanges(t *testing.T) []chapters.Range {
	t.Helper()
	frameMs := func(f int64) int64 { return f * chapters.FrameDenominator * 1000 / chapters.FrameNumerator }
	const skew = 10 // 半フレーム（16.7ms）より小さいずらし
	type pair struct{ startFrame, endFrame int64 }
	segments := []pair{{0, 90}, {150, 240}, {300, 390}, {450, 540}, {600, 690}, {750, 1197}}

	out := make([]chapters.Range, 0, len(segments))
	for i, seg := range segments {
		start := frameMs(seg.startFrame) - skew
		if i == 0 {
			start = 0
		}
		end := frameMs(seg.endFrame) + skew
		// ずらした結果が隣の境界に入れ替わらないことを確かめる（境界が交差すると
		// テストの前提が崩れる）。
		if got := chapters.MsToFrame(start); got != seg.startFrame {
			t.Fatalf("start %d ms quantizes to frame %d, want %d", start, got, seg.startFrame)
		}
		if got := chapters.MsToFrame(end); got != seg.endFrame {
			t.Fatalf("end %d ms quantizes to frame %d, want %d", end, got, seg.endFrame)
		}
		out = append(out, chapters.Range{StartMs: start, EndMs: end})
	}
	return out
}

// probeDurations は ffprobe で映像・音声・コンテナの長さを秒で返す。
func probeDurations(t *testing.T, ffprobe, path string) (video, audio, format float64) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "stream=codec_type,duration",
		"-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %s: %v (%s)", path, err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// csv=p=0 は `codec_type,duration`（duration が無い行は codec_type だけ）。
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 {
			continue
		}
		f, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "video":
			video = f
		case "audio":
			audio = f
		}
	}
	// コンテナ全体の長さは別に引く（stream= と format= を 1 回の呼び出しで
	// 混ぜると default=noprint_wrappers でキーが衝突する）。
	fout, err := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe format %s: %v (%s)", path, err, fout)
	}
	format, _ = strconv.ParseFloat(strings.TrimSpace(string(fout)), 64)
	if video == 0 || audio == 0 {
		t.Fatalf("ffprobe did not report both stream durations: %s", out)
	}
	return video, audio, format
}

// TestNextCutGeneration は置き換えの世代番号の導出を固定する。**1 世代目から
// `.g1` を付ける**ので、世代番号の無いパスからは 1 に戻る。
func TestNextCutGeneration(t *testing.T) {
	cases := []struct {
		prev string
		want int
	}{
		{"", 1},
		{"20240101/a_h264.mp4", 1},     // 世代の無いパス（cut でない版の名残）
		{"20240101/a_cut.g1.mp4", 2},   // 旧パスから +1
		{"20240101/a_cut.g11.mp4", 12}, // 2 桁でも読む
		{"20240101/a_cut.g0.mp4", 1},   // 0 は世代として無効 → 1
		{"20240101/a_cut.gx.mp4", 1},   // 読めない → 1
		{"20240101/a_cut.mp4", 1},      // 世代が付いていない cut 版
	}
	for _, tc := range cases {
		if got := nextCutGeneration(tc.prev, "cut"); got != tc.want {
			t.Errorf("nextCutGeneration(%q) = %d, want %d", tc.prev, got, tc.want)
		}
	}
	// プロファイル名に `.g<数字>` が含まれても、末尾の `.g<数字>` だけを読む。
	if got := nextCutGeneration("a_x.g9.g3.mp4", "x.g9"); got != 4 {
		t.Errorf("nextCutGeneration with a dotted profile name = %d, want 4", got)
	}
}

// TestEncodedRelPath_Generation は cut 版のパスが世代ごとに変わることを固定する。
// **同じパスへ上書きすると、生きている行の rel_path 部分一意索引と衝突し、
// ストレージ契約の「置くのは一回」も破る。**
func TestEncodedRelPath_Generation(t *testing.T) {
	got, err := EncodedRelPath("20240101/a.m2ts", "cut", "mp4", 1)
	if err != nil {
		t.Fatalf("EncodedRelPath: %v", err)
	}
	if want := "20240101/a_cut.g1.mp4"; got != want {
		t.Errorf("EncodedRelPath = %q, want %q", got, want)
	}
	got2, err := EncodedRelPath("20240101/a.m2ts", "cut", "mp4", 2)
	if err != nil {
		t.Fatalf("EncodedRelPath: %v", err)
	}
	if got2 == got {
		t.Errorf("generation 2 produced the same path as generation 1 (%q)", got2)
	}
	// generation 0 は従来のパス（cut でないプロファイル）。
	plain, err := EncodedRelPath("20240101/a.m2ts", "h264", "mp4", 0)
	if err != nil {
		t.Fatalf("EncodedRelPath: %v", err)
	}
	if want := "20240101/a_h264.mp4"; plain != want {
		t.Errorf("EncodedRelPath(generation 0) = %q, want %q", plain, want)
	}
}

// TestBuildFFmpegArgs_CutUsesFilterComplexNotVF は cut プロファイルの救済経路 argv を
// 固定する。`-vf` と `-filter_complex` を併用せず、`-map` はアプリが出し、
// output_format が無いときは `-vaapi_device` を使う。
func TestBuildFFmpegArgs_CutUsesFilterComplexNotVF(t *testing.T) {
	p := config.EncodeProfile{
		Name: "cut", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		Cut: true, Height: 480, Deinterlace: true,
		HWAccel: &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128"},
	}
	filter := &ffargs.CutFilterResult{FilterComplex: "[0:0]trim[vout]", VideoMap: "[vout]", AudioMap: "[aout]"}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4", false, filter)
	joined := strings.Join(args, " ")
	wantArgs := []string{
		"-hide_banner", "-nostats", "-y", "-vaapi_device", "/dev/dri/renderD128",
		"-i", "/in.m2ts", "-filter_complex", "[0:0]trim[vout]", "-map", "[vout]", "-map", "[aout]",
		"-c:v", "libx264", "-c:a", "aac", "-f", "mp4", "-progress", "pipe:1", "-loglevel", "error", "/out.mp4",
	}
	if !slices.Equal(args, wantArgs) {
		t.Errorf("rescue argv = %v, want %v", args, wantArgs)
	}

	if strings.Contains(joined, "-vf ") {
		t.Errorf("cut profile must not emit -vf: %s", joined)
	}
	if !strings.Contains(joined, "-filter_complex [0:0]trim[vout] -map [vout] -map [aout]") {
		t.Errorf("cut profile must map the filtergraph outputs: %s", joined)
	}
	if strings.Contains(joined, "-hwaccel vaapi") {
		t.Errorf("cut profile must not hardware-decode (trim cannot consume those frames): %s", joined)
	}
	if !strings.Contains(joined, "-vaapi_device /dev/dri/renderD128") {
		t.Errorf("cut profile with vaapi hwaccel must pass -vaapi_device: %s", joined)
	}

	// cut でないプロファイルは従来どおり -vf を使い、-map を出さない。
	plain := config.EncodeProfile{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac", Height: 480}
	plainArgs := strings.Join(BuildFFmpegArgs(plain, "/in.m2ts", "/out.mp4", false, nil), " ")
	if !strings.Contains(plainArgs, "-vf scale=-2:480") || strings.Contains(plainArgs, "-filter_complex") {
		t.Errorf("non-cut profile args changed: %s", plainArgs)
	}
}

// TestBuildFFmpegArgs_CutHWDecodeUsesPreInput は GPU 経路の入力 argv を固定する。
// HW decode オプションは -i より前で、-vaapi_device を使わない（filtergraph は
// TestBuildCutFilter_UploadMatchesDecodePath が固定する）。
func TestBuildFFmpegArgs_CutHWDecodeUsesPreInput(t *testing.T) {
	p := config.EncodeProfile{
		Name: "cut", Container: "mp4", VideoCodec: "h264_vaapi", AudioCodec: "aac",
		Cut: true, Height: 720, Deinterlace: true, Scaler: ffargs.ScalerVAAPI,
		HWAccel: &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"},
	}
	filter := &ffargs.CutFilterResult{
		FilterComplex: "[0:0]trim,concat[vcat];[vcat]deinterlace_vaapi,scale_vaapi=w=-2:h=720[vout];[acat]anull[aout]",
		VideoMap:      "[vout]",
		AudioMap:      "[aout]",
	}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4", false, filter)
	wantArgs := []string{
		"-hide_banner", "-nostats", "-y",
		"-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128", "-hwaccel_output_format", "vaapi",
		"-i", "/in.m2ts", "-filter_complex", "[0:0]trim,concat[vcat];[vcat]deinterlace_vaapi,scale_vaapi=w=-2:h=720[vout];[acat]anull[aout]",
		"-map", "[vout]", "-map", "[aout]",
		"-c:v", "h264_vaapi", "-c:a", "aac", "-f", "mp4", "-progress", "pipe:1", "-loglevel", "error", "/out.mp4",
	}
	if !slices.Equal(args, wantArgs) {
		t.Errorf("GPU cut argv = %v, want %v", args, wantArgs)
	}
}

// TestBuildCutFilter_UploadMatchesDecodePath は production の cut filter builder を通し、
// HW decode 経路では再 upload せず、CPU 救済経路では従来どおり upload することを固定する。
func TestBuildCutFilter_UploadMatchesDecodePath(t *testing.T) {
	ffprobe := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '0,video,1440,1080,\\n1,audio,,,2\\n'\n"), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	w := &EncodeWorker{FFprobe: ffprobe}
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	cases := []struct {
		name       string
		profile    config.EncodeProfile
		wantChain  string
		wantUpload bool
	}{
		{
			name: "hardware decode filters in VAAPI",
			profile: config.EncodeProfile{
				Cut: true, Height: 720, Deinterlace: true, Scaler: ffargs.ScalerVAAPI,
				HWAccel: &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"},
			},
			wantChain: "[vcat]deinterlace_vaapi,scale_vaapi=w=-2:h=720[vout]",
		},
		{
			name: "software decode rescue uploads after CPU filters",
			profile: config.EncodeProfile{
				Cut: true, Height: 720, Deinterlace: true,
				HWAccel: &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128"},
			},
			wantChain:  "[vcat]yadif,scale=-2:720,format=nv12,hwupload[vout]",
			wantUpload: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filter, err := w.buildCutFilter(context.Background(), c.profile, "/in.m2ts", keep)
			if err != nil {
				t.Fatalf("buildCutFilter: %v", err)
			}
			if !strings.Contains(filter.FilterComplex, c.wantChain) {
				t.Errorf("filter chain = %s, want %s", filter.FilterComplex, c.wantChain)
			}
			if gotUpload := strings.Contains(filter.FilterComplex, "hwupload"); gotUpload != c.wantUpload {
				t.Errorf("hwupload present = %v, want %v: %s", gotUpload, c.wantUpload, filter.FilterComplex)
			}
		})
	}
}

// TestCutIsCurrent_ComparesQuantizedRanges は「編集前の内容です」の判定が
// **量子化後の値どうし**で行われることを固定する。凍結した区間が 1ms でも違えば
// 「編集前」になり、逆に一致すれば作り直さない。
func TestCutIsCurrent_ComparesQuantizedRanges(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "cut/stale.m2ts", []string{"cut"}, []byte("data"))
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", "cut/stale_cut.g1.mp4")

	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 5000, EndMs: 6000}}
	if err := setFrozenCuts(t, pool, assetID, keep); err != nil {
		t.Fatalf("setFrozenCuts: %v", err)
	}

	q := sqlcgen.New(pool)
	current, err := cutIsCurrent(ctx, q, assetID, keep)
	if err != nil {
		t.Fatalf("cutIsCurrent: %v", err)
	}
	if !current {
		t.Error("the frozen ranges equal the current ones but were judged stale")
	}

	// 1ms だけ違う区間は「編集前」になる（量子化前の値で比べていないことの確認）。
	shifted := []chapters.Range{{StartMs: 0, EndMs: 1001}, {StartMs: 5000, EndMs: 6000}}
	stale, err := cutIsCurrent(ctx, q, assetID, shifted)
	if err != nil {
		t.Fatalf("cutIsCurrent: %v", err)
	}
	if stale {
		t.Error("a one-millisecond difference was not detected as stale")
	}

	// 全部カット（keep が空）は「作り直しても作れない」ので false。
	empty, err := cutIsCurrent(ctx, q, assetID, nil)
	if err != nil {
		t.Fatalf("cutIsCurrent: %v", err)
	}
	if empty {
		t.Error("an empty keep set must never count as current")
	}
}

// TestEncodeWorker_ReplacesCutAssetAndRemovesOldPath は置き換えの受け入れを
// 固定する: 新しい世代のパスに置き換わり、旧パスが消え、凍結区間が差し替わる。
func TestEncodeWorker_ReplacesCutAssetAndRemovesOldPath(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	// 2 秒の素材を用意する（実 ffmpeg でデコードする）。
	srcRel := "cut/replace.m2ts"
	srcAbs := filepath.Join(mediaDir, filepath.FromSlash(srcRel))
	if err := os.MkdirAll(filepath.Dir(srcAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	gen := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=30000/1001",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", srcAbs)

	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, srcRel, []string{"cut"}, []byte("placeholder"))
	// seedRecordingWithOriginal が置いた placeholder を、実 ffmpeg で作った本物の
	// 素材で上書きする（seed の後に走らせないと seed が空ファイルで潰す）。
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating source: %v (%s)", err, out)
	}
	// 番組の尺を素材に合わせる（Derive のタイムラインの終端は
	// program_duration_ms と既知の区間の終端の大きい方）。
	if _, err := pool.Exec(ctx, `UPDATE recordings SET program_duration_ms = 2000 WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("setting program duration: %v", err)
	}

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     ffmpeg,
		FFprobe:    filepath.Join(filepath.Dir(ffmpeg), "ffprobe"),
		Profiles:   config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}},
	}

	// 所有の行を作る（cut は確認済みを前提にする）。
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	// keep 区間 = 0..1 秒 と 1.5..2 秒（1 秒ぶんを切る）。境界は Derive の量子化を
	// 通るので、期待値も同じ関数で作る（リテラルで書くと粒度を変えたときに
	// テストの意味が変わる）。
	keep := []chapters.Range{
		{StartMs: 0, EndMs: chapters.QuantizeMs(1000)},
		{StartMs: chapters.QuantizeMs(1500), EndMs: 2000},
	}
	for _, s := range []chapters.Span{
		{StartMs: 1000, EndMs: 1500, Label: "CM", Cut: true},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
			 VALUES ($1, int8range($2, $3), $4, $5)`, recordingID, s.StartMs, s.EndMs, s.Label, s.Cut); err != nil {
			t.Fatalf("seeding chapter span: %v", err)
		}
	}

	// 旧世代のパスにファイルと行を置く（置き換えの前状態）。
	oldRel := "cut/replace_cut.g1.mp4"
	oldAbs := filepath.Join(mediaDir, filepath.FromSlash(oldRel))
	if err := os.WriteFile(oldAbs, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", oldRel)
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 2000}}); err != nil {
		t.Fatalf("setFrozenCuts: %v", err)
	}

	job := newEncodeJob(recordingID, "cut")
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("Work: %v", err)
	}

	profileName := "cut"
	row, err := sqlcgen.New(pool).GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID, Profile: &profileName,
	})
	if err != nil {
		t.Fatalf("loading encoded row: %v", err)
	}
	if row.RelPath != "cut/replace_cut.g2.mp4" {
		t.Errorf("rel_path = %q, want cut/replace_cut.g2.mp4 (the next generation)", row.RelPath)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, filepath.FromSlash(row.RelPath))); err != nil {
		t.Errorf("the new generation file is missing: %v", err)
	}
	if _, err := os.Stat(oldAbs); !os.IsNotExist(err) {
		t.Errorf("the replaced file %s still exists (err=%v)", oldAbs, err)
	}
	// 凍結した区間も差し替わる。
	current, err := cutIsCurrent(ctx, sqlcgen.New(pool), assetID, keep)
	if err != nil {
		t.Fatalf("cutIsCurrent: %v", err)
	}
	if !current {
		t.Error("media_asset_cuts was not replaced with the current keep ranges")
	}

	// 2 回目は冪等にスキップする（凍結区間が一致しているので作り直さない）。
	if err := w.Work(ctx, newEncodeJob(recordingID, "cut")); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	again, err := sqlcgen.New(pool).GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID, Profile: &profileName,
	})
	if err != nil {
		t.Fatalf("loading encoded row: %v", err)
	}
	if again.RelPath != row.RelPath {
		t.Errorf("a no-op re-run advanced the generation: %q -> %q", row.RelPath, again.RelPath)
	}
}

// seedEncodedAsset は active な encoded media_assets 行を 1 つ作って id を返す。
func seedEncodedAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile, relPath string) int64 {
	t.Helper()
	ctx := context.Background()
	profileName := profile
	assetID, err := sqlcgen.New(pool).UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID,
		Profile:     &profileName,
		RelPath:     relPath,
		SizeBytes:   1,
	})
	if err != nil {
		t.Fatalf("seeding encoded media_asset: %v", err)
	}
	return assetID
}

// setFrozenCuts は media_asset_cuts を直接書く（commitCutEncoded がやることの
// うち、行の upsert を除いた部分。テストが前状態を作るために使う）。
func setFrozenCuts(t *testing.T, pool *pgxpool.Pool, assetID int64, keep []chapters.Range) error {
	t.Helper()
	ranges, err := keepRangesParam(keep)
	if err != nil {
		return err
	}
	return replaceMediaAssetCuts(context.Background(), sqlcgen.New(pool), assetID, ranges)
}

// newEncodeJob は EncodeWorker.Work に渡す最小のジョブを組み立てる。
func newEncodeJob(recordingID int64, profile string) *river.Job[jobs.EncodeJobArgs] {
	return &river.Job[jobs.EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   jobs.EncodeJobArgs{RecordingID: recordingID, Profile: profile},
	}
}

// TestEnqueueMissingEncodes_CutProfilesWaitForReview は受け入れの「所有していない
// 録画では cut プロファイルの encode が投入されず、確認後に投入される」を固定する。
//
// **確認前に投入すると、誤検出のまま本編が削られ、原本がごみ箱を経由せずに消えて
// 取り返せなくなる。** cut でないプロファイルは同じ録画でも通常どおり投入される
// （この 2 つが同じループで分岐していることが要点）。
func TestEnqueueMissingEncodes_CutProfilesWaitForReview(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "cut/review.m2ts",
		[]string{"cut", "h264"}, []byte("payload"))

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cutProfiles := map[string]struct{}{"cut": {}}

	// 未確認: cut は投入されず、h264 だけが投入される。
	if err := EnqueueMissingEncodes(ctx, client, pool, recordingID, config.EncodeConfig{}, cutProfiles); err != nil {
		t.Fatalf("EnqueueMissingEncodes: %v", err)
	}
	if got := pendingEncodeProfiles(t, pool, recordingID); !slices.Equal(got, []string{"h264"}) {
		t.Fatalf("pending profiles before review = %v, want [h264] (cut must wait)", got)
	}

	// 確認した: cut も投入される。
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	if err := EnqueueMissingEncodes(ctx, client, pool, recordingID, config.EncodeConfig{}, cutProfiles); err != nil {
		t.Fatalf("second EnqueueMissingEncodes: %v", err)
	}
	if got := pendingEncodeProfiles(t, pool, recordingID); !slices.Equal(got, []string{"cut", "h264"}) {
		t.Fatalf("pending profiles after review = %v, want [cut h264]", got)
	}
}

// pendingEncodeProfiles は投入済み（pending/available/running）の encode ジョブの
// プロファイル名をソートして返す。
func pendingEncodeProfiles(t *testing.T, pool *pgxpool.Pool, recordingID int64) []string {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewRiverClient(t, pool)
	rows := testutil.MustListRiverJobs(t, ctx, client, river.NewJobListParams().
		Kinds((jobs.EncodeJobArgs{}).Kind()).
		States(rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled))
	var out []string
	for _, row := range rows {
		var args jobs.EncodeJobArgs
		if err := json.Unmarshal(row.EncodedArgs, &args); err != nil {
			t.Fatalf("decoding encode job %d args: %v", row.ID, err)
		}
		if args.RecordingID == recordingID {
			out = append(out, args.Profile)
		}
	}
	sort.Strings(out)
	return out
}

// TestIngestWorker_ClampsCutOnlyProfileSelection は受け入れの「マージ結果が cut
// のみになったら cut が落ちる」を、4 経路のうちの ingest の凍結で固定する。
//
// ルール単独（cut + h264）と override 単独（h264 を明示的に打ち消す）はどちらも
// 禁則を満たしているが、マージ結果は cut だけになる。**書く前に安全側へ倒さないと
// cut だけの録画が凍結され、確認に再生が要り・再生に encode が要り・encode に確認が
// 要る循環に落ちる。**
func TestIngestWorker_ClampsCutOnlyProfileSelection(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000007)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "cut だけになる予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["cut","h264"]}`)

	overrides, err := json.Marshal(map[string]any{
		// h264 を明示的に打ち消す → 実効値は cut だけ。
		"encodeProfiles": []string{"cut"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID, Overrides: overrides,
	}); err != nil {
		t.Fatalf("setting override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-cut-only", programID)

	srv := newFullTransferServer(t, makeTSData(20), "test/policy-cut-only.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		Pool:          pool,
		StallTimeout:  5 * time.Second,
		// cut: true のプロファイル名（本番では config から注入される）。
		CutProfiles: map[string]struct{}{"cut": {}},
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-cut-only"},
	}
	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always", keepOriginal)
	}
	// cut が落ちて空になる（cut でないプロファイルが 1 つも無いので、安全側は
	// 「エンコードしない」）。空のまま凍結されることが要点で、cut が残っては
	// ならない。
	for _, p := range profiles {
		if p == "cut" {
			t.Errorf("encode_profiles = %v, want cut dropped", profiles)
		}
	}
}

// TestIngestWorker_PreservesCutOnlyProfileSelectionWithLive は原本 HLS が使える構成で
// 凍結時のクランプが cut profile を落とさないことを固定する。
func TestIngestWorker_PreservesCutOnlyProfileSelectionWithLive(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000008)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "live 有効で cut だけになる予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["cut","h264"]}`)

	overrides, err := json.Marshal(map[string]any{"encodeProfiles": []string{"cut"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID, Overrides: overrides,
	}); err != nil {
		t.Fatalf("setting override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-cut-only-live", programID)

	srv := newFullTransferServer(t, makeTSData(20), "test/policy-cut-only-live.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)
	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		Pool:          pool,
		StallTimeout:  5 * time.Second,
		CutProfiles:   map[string]struct{}{"cut": {}},
		LiveEnabled:   true,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-cut-only-live"},
	}
	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	_, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if len(profiles) != 1 || profiles[0] != "cut" {
		t.Errorf("encode_profiles = %v, want [cut] with live enabled", profiles)
	}
}

// TestEnqueueCut_AllCutRecordingEnqueuesNothing は「全区間カットの録画は、確認済みでも
// cut のジョブを投入しない」を、ヒント経路（EnqueueMissingEncodes）と定期 reconcile の
// 両方で固定する。投入しても loadCutContext が "has no keep ranges" で必ず失敗し、
// reconcile のたびに失敗ジョブが積まれる。一部だけ切る録画（partial）は両経路で投入される。
func TestEnqueueCut_AllCutRecordingEnqueuesNothing(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	seed := func(rel string, spans ...chapters.Span) int64 {
		id := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"cut"}, []byte("x"))
		if _, err := pool.Exec(ctx, `UPDATE recordings SET program_duration_ms = 1800000 WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
		for _, s := range spans {
			if _, err := pool.Exec(ctx,
				`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range($2, $3), $4, true)`,
				id, s.StartMs, s.EndMs, "CM"); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	allCut := seed("cut/allcut.m2ts", chapters.Span{StartMs: 0, EndMs: 1800000})
	partial := seed("cut/partial.m2ts", chapters.Span{StartMs: 500, EndMs: 1000})

	client, err := NewClient(pool, NewWorkers(&Deps{Pool: pool}), ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cut := map[string]struct{}{"cut": {}}

	// ヒント経路。
	for _, id := range []int64{allCut, partial} {
		if err := EnqueueMissingEncodes(ctx, client, pool, id, config.EncodeConfig{}, cut); err != nil {
			t.Fatalf("EnqueueMissingEncodes(%d): %v", id, err)
		}
	}
	if got := pendingEncodeProfiles(t, pool, allCut); len(got) != 0 {
		t.Errorf("hint path: all-cut recording pending = %v, want none", got)
	}
	if got := pendingEncodeProfiles(t, pool, partial); !slices.Equal(got, []string{"cut"}) {
		t.Errorf("hint path: partial recording pending = %v, want [cut]", got)
	}

	// 定期 reconcile 経路（ジョブを消してから回す）。
	testutil.MustDeleteRiverJobsOfKind(t, ctx, pool, (jobs.EncodeJobArgs{}).Kind())
	cfg := config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}}
	runEncodeReconcilePass(t, pool, &EncodeReconcileWorker{Pool: pool, Profiles: cfg})
	if got := pendingEncodeProfiles(t, pool, allCut); len(got) != 0 {
		t.Errorf("reconcile path: all-cut recording pending = %v, want none", got)
	}
	if got := pendingEncodeProfiles(t, pool, partial); !slices.Equal(got, []string{"cut"}) {
		t.Errorf("reconcile path: partial recording pending = %v, want [cut]", got)
	}
}

// TestCutEncode_RealFFmpeg_AudioLeadsVideo は「音声が映像より 300ms 早く始まる TS でも、
// 映像と音声が同じ時間軸（チャプターの原点 = 入力の最早 start_time）で切られる」を測る。
//
// 放送 TS は音声が映像より先に始まることが多い。映像を「最初の映像フレームから数えた
// フレーム番号」で切ると、音声（入力の最早 start_time 基準の秒）との間に開始差ぶんの
// ずれが全区間で出る。source では PTS = 3.0s で映像がフラッシュし、同じ PTS で音声が
// ビープする。keep（[0,1s) と [2s,4s)）で切った出力では、両方が 2.0s に出るはず。
func TestCutEncode_RealFFmpeg_AudioLeadsVideo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ts")
	out := filepath.Join(dir, "out.mp4")

	// 映像は 0.3s 遅れて始まる（-itsoffset で PTS をずらす）。フラッシュは映像自身の
	// 時刻 2.7-2.9s = PTS 3.0-3.2s。ビープは音声の PTS 3.0-3.2s。
	gen := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-itsoffset", "0.3", "-f", "lavfi", "-i", "color=c=black:s=64x64:r=30000/1001,drawbox=x=0:y=0:w=64:h=64:c=white:t=fill:enable='between(t,2.7,2.9)'",
		"-f", "lavfi", "-i", "aevalsrc='0.8*sin(2*PI*1000*t)*between(t,3.0,3.2)':s=48000:c=mono",
		"-t", "5", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-c:a", "aac",
		"-f", "mpegts", src)
	if b, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating source: %v (%s)", err, b)
	}
	// 前提の確認: 音声が映像より 0.25s 以上早く始まっている。
	starts := map[string]float64{}
	pout, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=index,codec_type,start_time", "-of", "csv=p=0", src).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v (%s)", err, pout)
	}
	var vIdx, aIdx int
	for _, line := range strings.Split(strings.TrimSpace(string(pout)), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 3 {
			continue
		}
		idx, _ := strconv.Atoi(f[0])
		st, _ := strconv.ParseFloat(f[2], 64)
		starts[f[1]] = st
		switch f[1] {
		case "video":
			vIdx = idx
		case "audio":
			aIdx = idx
		}
	}
	if lead := starts["video"] - starts["audio"]; lead < 0.25 {
		t.Fatalf("source audio leads video by %.3fs, want >= 0.25s (start_time %v)", lead, starts)
	}

	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 4000}}
	for i := range keep {
		keep[i].StartMs = chapters.QuantizeMs(keep[i].StartMs)
		keep[i].EndMs = chapters.QuantizeMs(keep[i].EndMs)
	}
	filter, err := ffargs.CutFilterComplex(keep, vIdx, aIdx, ffargs.ScalerSoftware, 0, false, false)
	if err != nil {
		t.Fatalf("CutFilterComplex: %v", err)
	}
	args := BuildFFmpegArgs(cutFFmpegProfile(), src, out, false, &filter)
	enc := exec.Command(ffmpeg, args...)
	enc.Env = append(os.Environ(), "PATH="+filepath.Dir(ffmpeg)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if b, err := enc.CombinedOutput(); err != nil {
		t.Fatalf("cut encode failed: %v (%s)\nargs: %v", err, b, args)
	}

	// 出力の映像: 最初に明るくなるフレームの表示時刻（showinfo の pts_time。映像が
	// 音声より遅れて始まる出力ではフレーム番号から時刻を逆算できない）。
	vinfo, err := exec.Command(ffmpeg, "-hide_banner", "-i", out, "-map", "0:v:0",
		"-vf", "showinfo", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("decoding video: %v (%s)", err, vinfo)
	}
	flash := -1.0
	for _, line := range strings.Split(string(vinfo), "\n") {
		i := strings.Index(line, "pts_time:")
		j := strings.Index(line, "mean:[")
		if i < 0 || j < 0 {
			continue
		}
		pts, _ := strconv.ParseFloat(strings.Fields(line[i+len("pts_time:"):])[0], 64)
		y, _ := strconv.Atoi(strings.Fields(strings.NewReplacer("[", " ", "]", " ").Replace(line[j+len("mean:"):]))[0])
		if y > 128 {
			flash = pts
			break
		}
	}
	// 出力の音声: 最初に振幅が立つサンプルの時刻。
	araw, err := exec.Command(ffmpeg, "-v", "error", "-i", out, "-map", "0:a:0",
		"-ac", "1", "-ar", "48000", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatalf("decoding audio: %v", err)
	}
	beep := -1.0
	for i := 0; i+2 <= len(araw); i += 2 {
		v := int16(uint16(araw[i]) | uint16(araw[i+1])<<8)
		if v > 8000 || v < -8000 {
			beep = float64(i/2) / 48000
			break
		}
	}
	if flash < 0 || beep < 0 {
		t.Fatalf("marker not found in output: flash=%.3f beep=%.3f", flash, beep)
	}
	t.Logf("flash=%.3fs beep=%.3fs skew=%.0fms (want both ~2.0s)", flash, beep, (flash-beep)*1000)
	tolerance := 2 * float64(chapters.FrameDenominator) / float64(chapters.FrameNumerator)
	if d := flash - beep; d > tolerance || d < -tolerance {
		t.Errorf("video flash at %.3fs and audio beep at %.3fs differ by %.0fms (> two frames %.0fms)", flash, beep, d*1000, tolerance*1000)
	}
	if d := beep - 2.0; d > tolerance || d < -tolerance {
		t.Errorf("beep at %.3fs, want 2.0s (chapter axis)", beep)
	}
}
