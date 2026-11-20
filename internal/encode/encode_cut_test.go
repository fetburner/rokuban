package encode

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/ffargs"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testexec"
	"github.com/fetburner/rokuban/internal/testutil"
)

func int64ptr(value int64) *int64 { return &value }

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
	t.Parallel()
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
	testutil.RequireNoError(t, err, "CutFilterComplex")
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

// TestPlanEncodePublishUsesPersistedGeneration は世代の選択と、その世代の rel_path を
// 行ごとに固定する。
func TestPlanEncodePublishUsesPersistedGeneration(t *testing.T) {
	t.Parallel()
	keep := []chapters.Range{{StartMs: 0, EndMs: 60_000}}
	tests := []struct {
		name        string
		input       store.EncodedPublishInput
		state       store.EncodedPublishState
		want        int64
		wantRelPath string
	}{
		{
			name:  "first uncut output",
			input: store.EncodedPublishInput{Profile: "h264"},
			state: store.EncodedPublishState{Desired: true},
			want:  0, wantRelPath: "20240101/a_h264.mp4",
		},
		{
			name:  "first cut output",
			input: store.EncodedPublishInput{Profile: "cut", Cut: true, KeepRanges: keep},
			state: store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keep},
			want:  1, wantRelPath: "20240101/a_cut.g1.mp4",
		},
		{
			name:  "active replacement",
			input: store.EncodedPublishInput{Profile: "cut", Cut: true, KeepRanges: keep, HasRebuildRequest: true, RequestedKeep: keep},
			state: store.EncodedPublishState{
				Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keep,
				EncodedAsset: &store.EncodedAssetObservation{Active: true, DerivedGeneration: 7, KeepRanges: []chapters.Range{{StartMs: 0, EndMs: 30_000}}},
			},
			want: 8, wantRelPath: "20240101/a_cut.g8.mp4",
		},
		{
			name:  "deleted uncut tombstone",
			input: store.EncodedPublishInput{Profile: "h264"},
			state: store.EncodedPublishState{
				Desired:      true,
				EncodedAsset: &store.EncodedAssetObservation{DerivedGeneration: 4},
			},
			want: 5, wantRelPath: "20240101/a_h264.u5.mp4",
		},
		{
			name:  "generation reserved by an interrupted first attempt",
			input: store.EncodedPublishInput{Profile: "h264"},
			state: store.EncodedPublishState{Desired: true, MaxCopyGeneration: int64ptr(0)},
			want:  1, wantRelPath: "20240101/a_h264.u1.mp4",
		},
		{
			name:  "pending generation after the committed copy",
			input: store.EncodedPublishInput{Profile: "cut", Cut: true, KeepRanges: keep, HasRebuildRequest: true, RequestedKeep: keep},
			state: store.EncodedPublishState{
				Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keep,
				EncodedAsset:      &store.EncodedAssetObservation{Active: true, DerivedGeneration: 4, KeepRanges: []chapters.Range{{StartMs: 0, EndMs: 30_000}}},
				MaxCopyGeneration: int64ptr(7),
			},
			want: 8, wantRelPath: "20240101/a_cut.g8.mp4",
		},
		{
			name:  "deleted cut tombstone",
			input: store.EncodedPublishInput{Profile: "cut", Cut: true, KeepRanges: keep, HasRebuildRequest: true, RequestedKeep: keep},
			state: store.EncodedPublishState{
				Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keep,
				EncodedAsset: &store.EncodedAssetObservation{DerivedGeneration: 12},
			},
			want: 13, wantRelPath: "20240101/a_cut.g13.mp4",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := planEncodePublishFromState(tc.input, tc.state)
			if plan.skip || plan.generation != tc.want {
				t.Fatalf("plan = {skip:%v generation:%d}, want generation %d", plan.skip, plan.generation, tc.want)
			}
			got, err := EncodedRelPath("20240101/a.m2ts", tc.input.Profile, "mp4", int(plan.generation), tc.input.Cut)
			testutil.RequireNoError(t, err, "EncodedRelPath")
			if got != tc.wantRelPath {
				t.Errorf("EncodedRelPath = %q, want %q", got, tc.wantRelPath)
			}
		})
	}
}

// TestBuildFFmpegArgs_CutUsesFilterComplexNotVF は cut プロファイルの救済経路 argv を
// 固定する。`-vf` と `-filter_complex` を併用せず、`-map` はアプリが出し、
// output_format が無いときは `-vaapi_device` を使う。
func TestBuildFFmpegArgs_CutUsesFilterComplexNotVF(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	ffprobe := filepath.Join(t.TempDir(), "ffprobe")
	testexec.WriteExecutable(t, ffprobe, []byte("#!/bin/sh\nprintf '0,video,1440,1080,\\n1,audio,,,2\\n'\n"))
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
			testutil.RequireNoError(t, err, "buildCutFilter")
			if !strings.Contains(filter.FilterComplex, c.wantChain) {
				t.Errorf("filter chain = %s, want %s", filter.FilterComplex, c.wantChain)
			}
			if gotUpload := strings.Contains(filter.FilterComplex, "hwupload"); gotUpload != c.wantUpload {
				t.Errorf("hwupload present = %v, want %v: %s", gotUpload, c.wantUpload, filter.FilterComplex)
			}
		})
	}
}

// TestEncodeWorker_ReplacesCutAssetAndRemovesOldPath は明示要求による置き換えを
// 固定する: 新しい世代のパスに置き換わり、旧パスが消え、凍結区間が差し替わる。
func TestEncodeWorker_ReplacesCutAssetAndRemovesOldPath(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
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
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(srcAbs), 0o755), "MkdirAll")
	gen := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=30000/1001",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", srcAbs)

	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, srcRel, []string{"cut"}, []byte("placeholder"))
	// prepareEncodeInput が置いた placeholder を、実 ffmpeg で作った本物の
	// 素材で上書きする（seed の後に走らせないと seed が空ファイルで潰す）。
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating source: %v (%s)", err, out)
	}
	// 番組の尺を素材に合わせる（Derive のタイムラインの終端は
	// program_duration_ms と既知の区間の終端の大きい方）。
	testutil.MustExec(t, pool, "setting program duration", `UPDATE recordings SET program_duration_ms = 2000 WHERE id = $1`, recordingID)

	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Stores:    Stores{Attempts: pgstoretest.Media(pool), Progress: pgstoretest.Media(pool)},

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		ScratchDir:      scratchDir,
		FFmpeg:          ffmpeg,
		FFprobe:         filepath.Join(filepath.Dir(ffmpeg), "ffprobe"),
		Profiles:        config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}},
	}

	// 所有の行を作る（cut は確認済みを前提にする）。
	dbtest.AdoptChapters(t, pool, recordingID)
	// keep 区間 = 0..1 秒 と 1.5..2 秒（1 秒ぶんを切る）。境界は Derive の量子化を
	// 通るので、期待値も同じ関数で作る（リテラルで書くと粒度を変えたときに
	// テストの意味が変わる）。
	keep := []chapters.Range{
		{StartMs: 0, EndMs: chapters.QuantizeMs(1000)},
		{StartMs: chapters.QuantizeMs(1500), EndMs: 2000},
	}
	dbtest.ChapterSpan(t, pool, recordingID, func(p *dbtest.ChapterSpanParams) {
		p.StartMs, p.EndMs = 1000, 1500
	})

	// 旧世代のパスにファイルと行を置く（置き換えの前状態）。
	oldRel := "cut/replace_cut.g1.mp4"
	oldAbs := filepath.Join(mediaDir, filepath.FromSlash(oldRel))
	testutil.RequireNoError(t, os.WriteFile(oldAbs, []byte("old"), 0o644), "WriteFile")
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", oldRel)
	dbtest.SetMediaAssetCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 2000}})

	requestedKeep := seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)
	job := newCutEncodeJob(recordingID)
	job.RequestedCutKeep = requestedKeep
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work")

	row := encodedCutAsset(t, pool, recordingID)
	if row == nil {
		t.Fatal("encoded row missing after the cut job")
	}
	var relPath string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT rel_path FROM active_media_asset_copies($1, NULL)`, row.ID).Scan(&relPath), "loading the current copy")
	if relPath != "cut/replace_cut.g2.mp4" || row.DerivedGeneration != 2 {
		t.Errorf("current copy = %q generation %d, want cut/replace_cut.g2.mp4 generation 2", relPath, row.DerivedGeneration)
	}
	assertMediaAssetCopy(t, pool, row.ID, relPath)
	if _, err := os.Stat(filepath.Join(mediaDir, filepath.FromSlash(relPath))); err != nil {
		t.Errorf("the new generation file is missing: %v", err)
	}
	if _, err := os.Stat(oldAbs); err != nil {
		t.Errorf("the old generation file %s should remain until transfer overlap expires (err=%v)", oldAbs, err)
	}
	// 凍結した区間も差し替わる。
	current, err := frozenKeepRanges(ctx, pool, recordingID, "cut")
	testutil.RequireNoError(t, err, "frozenKeepRanges")
	if !slices.Equal(current, keep) {
		t.Errorf("media_asset_cuts ranges = %v, want %v", current, keep)
	}

	// 2 回目は冪等にスキップする（凍結区間が一致しているので作り直さない）。
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, newCutEncodeJob(recordingID)), "second Work")
	again := encodedCutAsset(t, pool, recordingID)
	if again == nil {
		t.Fatal("encoded row missing after the no-op re-run")
	}
	if again.DerivedGeneration != row.DerivedGeneration {
		t.Errorf("a no-op re-run advanced the generation: %d -> %d", row.DerivedGeneration, again.DerivedGeneration)
	}
}

// seedEncodedAsset は active な encoded media_assets 行を 1 つ作って id を返す。
func seedEncodedAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile, relPath string, generations ...int64) int64 {
	t.Helper()
	profileName := profile
	generation := testLegacyDerivedGeneration(relPath)
	if len(generations) > 0 {
		generation = generations[0]
	}
	assetID := dbtest.DerivedMediaAsset(t, pool, recordingID, "encoded", func(p *dbtest.DerivedMediaAssetParams) {
		p.Profile = &profileName
		p.Generation = generation
	})
	dbtest.MediaAssetCopy(t, pool, assetID, func(p *dbtest.MediaAssetCopyParams) {
		p.RelPath = relPath
		p.Generation = generation
	})
	return assetID
}

func testLegacyDerivedGeneration(relPath string) int64 {
	stem := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	_, generation, ok := config.SplitCutGeneration(stem)
	if !ok {
		_, generation, ok = config.SplitUncutGeneration(stem)
	}
	if !ok {
		return 0
	}
	return int64(generation)
}

func TestPrepareEncodePublishDoesNotReplaceActiveCutWithoutRequest(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/unrequested-replacement.m2ts", []string{"cut"}, []byte("original"))
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.ChapterSpan(t, pool, recordingID, func(p *dbtest.ChapterSpanParams) {
		p.StartMs, p.EndMs = 400000, 800000
	})
	currentKeep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(currentKeep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", currentKeep, owned, err)
	}
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", "rebuild/stale_cut.mp4")
	oldKeep := []chapters.Range{{StartMs: 0, EndMs: 200000}, {StartMs: 300000, EndMs: 1800000}}
	dbtest.SetMediaAssetCuts(t, pool, assetID, oldKeep)
	_, err = pgstoretest.Media(pool).Begin(ctx, recordingID, "cut", jobs.EncodeAttemptLimit)
	testutil.RequireNoError(t, err, "seeding running encode attempt")

	observed, err := planEncodePublish(ctx, pool, recordingID, "cut", &cutContext{keep: currentKeep})
	testutil.RequireNoError(t, err, "observing stale encoded asset")
	if !observed.skip || observed.skipReason != store.EncodedPublishSkipActiveCutProtected {
		t.Fatalf("observed plan = %+v, want an ordinary cut to skip an active asset", observed)
	}

	publishedCalls := 0
	published, err := pgstoretest.Media(pool).PublishEncoded(ctx, store.EncodedPublishInput{
		RecordingID:        recordingID,
		Profile:            "cut",
		Attempt:            1,
		RelPath:            "rebuild/stale_cut.g2.mp4",
		SizeBytes:          1,
		ObservedGeneration: observed.observedGeneration,
		Cut:                true,
		KeepRanges:         currentKeep,
	}, func() error { publishedCalls++; return nil })
	testutil.RequireNoError(t, err, "publishing ordinary cut encode")
	if published || publishedCalls != 0 {
		t.Fatalf("publish = %v callback calls = %d; active cut must not be replaced without a rebuild request", published, publishedCalls)
	}
}

func seedEncodedAssetRebuildRequest(t *testing.T, pool *pgxpool.Pool, recordingID int64, keep []chapters.Range) json.RawMessage {
	t.Helper()
	dbtest.EncodedAssetRebuildRequest(t, pool, recordingID, "cut", keep)
	requestedKeep, err := json.Marshal(keep)
	testutil.RequireNoError(t, err, "encoding rebuild keep ranges")
	return requestedKeep
}

// encodedCutAsset は録画の "cut" プロファイルの encoded 行を契約の GetEncodedPublishState で読む。行が無ければ nil を返す。
func encodedCutAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64) *store.EncodedAssetObservation {
	t.Helper()
	state, err := pgstoretest.Media(pool).GetEncodedPublishState(context.Background(), recordingID, "cut", false, false)
	testutil.RequireNoError(t, err, "loading encoded publish state")
	return state.EncodedAsset
}

// rebuildRequestExists は録画の "cut" プロファイルの作り直し要求行があるかを返す。
func rebuildRequestExists(t *testing.T, pool *pgxpool.Pool, recordingID int64) bool {
	t.Helper()
	state, err := pgstoretest.Media(pool).GetEncodedPublishState(context.Background(), recordingID, "cut", false, false)
	if err != nil {
		t.Fatalf("loading rebuild request for recording %d: %v", recordingID, err)
	}
	return state.HasRebuildRequest
}

// TestEncodeWorker_SatisfiedRebuildRequestSkipsWithoutRunningFFmpegAndKeepsRow は、既に
// 要求の keep を凍結した版があるとき、ffmpeg を起動せず、要求行も消さないことを固定する。
func TestEncodeWorker_SatisfiedRebuildRequestSkipsWithoutRunningFFmpegAndKeepsRow(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "rebuild/noop.m2ts", []string{"cut"}, []byte("original"))
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.ChapterSpan(t, pool, recordingID)
	keep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", "rebuild/noop_cut.mp4")
	dbtest.SetMediaAssetCuts(t, pool, assetID, keep)
	requestedKeep := seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)
	profile := cutFFmpegProfile()
	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Stores:    Stores{Attempts: pgstoretest.Media(pool), Progress: pgstoretest.Media(pool)},

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		ScratchDir:      scratchDir,
		FFmpeg:          filepath.Join(t.TempDir(), "must-not-run-ffmpeg"),
		Profiles:        config.EncodeConfig{Profiles: []config.EncodeProfile{profile}},
	}
	job := newCutEncodeJob(recordingID)
	job.RequestedCutKeep = requestedKeep
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work")
	if !rebuildRequestExists(t, pool, recordingID) {
		t.Fatal("satisfied rebuild request is missing; want it kept (only api removes it)")
	}
	if got := unsatisfiedRebuildCount(t, pool, recordingID); got != 0 {
		t.Fatalf("unsatisfied rebuilds = %d, want 0", got)
	}
	asset := encodedCutAsset(t, pool, recordingID)
	if asset == nil || asset.ID != assetID {
		t.Fatalf("encoded asset after no-op = %+v; want existing asset %d", asset, assetID)
	}
}

// TestEncodeWorker_SkipsRequestBoundJobWhenRequestIsGone は、要求に束縛されたジョブが、
// 開始前に要求行が消えていれば（公開・外す操作で消費済み）ffmpeg を起動せずに終わる
// ことを固定する。worker はエンコード前に、ロックを取らない読み取りで行の不在を判断し、
// 公開 tx では要求行をロック下で読み直す。
func TestEncodeWorker_SkipsRequestBoundJobWhenRequestIsGone(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "rebuild/gone.m2ts", []string{"cut"}, []byte("original"))
	dbtest.AdoptChapters(t, pool, recordingID)
	keep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	requestJSON := seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)
	testutil.MustExec(t, pool, "consuming rebuild request", `DELETE FROM encoded_asset_rebuild_requests WHERE recording_id = $1 AND profile = $2`, recordingID, "cut")
	profile := cutFFmpegProfile()
	w := &EncodeWorker{
		Publisher: pgstoretest.Media(pool),
		Stores:    Stores{Attempts: pgstoretest.Media(pool), Progress: pgstoretest.Media(pool)},

		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		ScratchDir:      scratchDir,
		FFmpeg:          filepath.Join(t.TempDir(), "must-not-run-ffmpeg"),
		Profiles:        config.EncodeConfig{Profiles: []config.EncodeProfile{profile}},
	}
	job := newCutEncodeJob(recordingID)
	job.RequestedCutKeep = requestJSON
	if err := jobqfaketest.RunWorkerOnce(ctx, w, job); err != nil {
		t.Fatalf("Work = %v, want nil (skip without running ffmpeg)", err)
	}
	if asset := encodedCutAsset(t, pool, recordingID); asset != nil {
		t.Fatalf("encoded asset after skipped job = %+v, want no row", asset)
	}
}

// TestEncodeWorker_RequestBoundJobDrawsRequestedKeepAfterChapterEdit は、要求に束縛された
// ジョブが、押した後にチャプターが編集されていても取り消されず、押した時点の keep で
// 描くことを固定する（エンコード前の判定は要求行の keep と照合し、現在の章とは比べない）。
func TestEncodeWorker_RequestBoundJobDrawsRequestedKeepAfterChapterEdit(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/edited.m2ts", []string{"cut"}, []byte("original"))
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.ChapterSpan(t, pool, recordingID)
	requestedKeep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(requestedKeep) == 0 {
		t.Fatalf("initial currentCutKeep = %v, owned %v, err %v", requestedKeep, owned, err)
	}
	requestJSON := seedEncodedAssetRebuildRequest(t, pool, recordingID, requestedKeep)
	testutil.MustExec(t, pool, "editing chapters after request", `UPDATE recording_chapter_spans SET span = int8range(500000, 900000) WHERE recording_id = $1`, recordingID)
	currentKeep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || chapters.SameRanges(currentKeep, requestedKeep) {
		t.Fatalf("edited currentKeep = %v, owned %v, err %v; want different keep", currentKeep, owned, err)
	}
	w := &EncodeWorker{Stores: Stores{Attempts: pgstoretest.Media(pool), Progress: pgstoretest.Media(pool)}, Publisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}}}
	job := newCutEncodeJob(recordingID)
	job.RequestedCutKeep = requestJSON
	_, cut, expected, skip, err := w.observeEncodePlan(ctx, job, slog.Default())
	if err != nil || len(expected) == 0 || skip || cut == nil {
		t.Fatalf("observeEncodePlan = cut %v, expected %v, skip %v, err %v; want a job that proceeds", cut, expected, skip, err)
	}
	if !chapters.SameRanges(cut.keep, requestedKeep) {
		t.Fatalf("cut keep = %v, want the requested %v (not the edited %v)", cut.keep, requestedKeep, currentKeep)
	}
}

// newCutEncodeJob は EncodeWorker.Work に渡す cut プロファイルの最小ジョブを組み立てる。
func newCutEncodeJob(recordingID int64) jobs.EncodeJobArgs {
	return jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "cut"}
}

// TestEncodeReconcile_CutProfilesWaitForReview は受け入れの「所有していない
// 録画では cut プロファイルの encode が投入されず、確認後に投入される」を固定する。
//
// **確認前に投入すると、誤検出のまま本編が削られ、原本がごみ箱を経由せずに消えて
// 取り返せなくなる。** cut でないプロファイルは同じ録画でも通常どおり投入される
// （この 2 つが同じループで分岐していることが要点）。
func TestEncodeReconcile_CutProfilesWaitForReview(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cut/review.m2ts",
		[]string{"cut", "h264"}, []byte("payload"))

	cfg := config.EncodeConfig{Profiles: []config.EncodeProfile{
		cutFFmpegProfile(), {Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}
	w := &EncodeReconcileWorker{Profiles: cfg, Media: pgstoretest.Media(pool)}

	// 未確認: cut は投入されず、h264 だけが投入される。
	enqueuer := runEncodeReconcilePass(t, w)
	if got := pendingEncodeProfiles(t, enqueuer, recordingID); !slices.Equal(got, []string{"h264"}) {
		t.Fatalf("pending profiles before review = %v, want [h264] (cut must wait)", got)
	}

	// 確認した: cut も投入される。
	dbtest.AdoptChapters(t, pool, recordingID)
	enqueuer.Reset()
	runEncodeReconcilePass(t, w)
	if got := pendingEncodeProfiles(t, enqueuer, recordingID); !slices.Equal(got, []string{"cut", "h264"}) {
		t.Fatalf("pending profiles after review = %v, want [cut h264]", got)
	}
}

// TestEncodeReconcile_DoesNotRebuildStaleCutWhenAddingProfile keeps a stale cut
// asset complete while a different profile is added to desired.
func TestEncodeReconcile_DoesNotRebuildStaleCutWhenAddingProfile(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cut/add-profile-stale.m2ts",
		[]string{"cut", "h264"}, []byte("payload"))
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.ChapterSpan(t, pool, recordingID)
	cutAssetID := seedEncodedAsset(t, pool, recordingID, "cut", "cut/add-profile_stale_cut.g1.mp4")
	frozen, _, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	testutil.RequireNoError(t, err, "deriving initial keep ranges")
	dbtest.SetMediaAssetCuts(t, pool, cutAssetID, frozen)
	label := "CM2"
	dbtest.ChapterSpan(t, pool, recordingID, func(p *dbtest.ChapterSpanParams) {
		p.StartMs, p.EndMs, p.Label = 1200000, 1300000, &label
	})
	current, _, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	testutil.RequireNoError(t, err, "deriving edited keep ranges")
	if slices.Equal(frozen, current) {
		t.Fatal("test setup did not make the active cut asset stale")
	}
	cfg := config.EncodeConfig{Profiles: []config.EncodeProfile{
		cutFFmpegProfile(), {Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}
	enqueuer := runEncodeReconcilePassArgs(t, &EncodeReconcileWorker{Profiles: cfg, Media: pgstoretest.Media(pool)},
		jobs.EncodeReconcileArgs{RecordingID: recordingID})
	if got := pendingEncodeProfiles(t, enqueuer, recordingID); !slices.Equal(got, []string{"h264"}) {
		t.Fatalf("pending profiles after adding h264 = %v, want [h264] (stale cut requires an explicit rebuild command)", got)
	}
}

// pendingEncodeProfiles は記録した encode 投入要求のプロファイル名をソートして返す。
func pendingEncodeProfiles(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, recordingID int64) []string {
	t.Helper()
	var out []string
	for _, args := range recordedArgs[jobs.EncodeJobArgs](t, enqueuer.CallsOfKind((jobs.EncodeJobArgs{}).Kind())) {
		if args.RecordingID == recordingID {
			out = append(out, args.Profile)
		}
	}
	sort.Strings(out)
	return out
}

// TestEncodeReconcile_AllCutRecordingEnqueuesNothing は「全区間カットの録画は、
// 確認済みでも cut のジョブを投入しない」を固定する。投入しても observeEncodePlan が
// "has no keep ranges" で失敗し続けるため、一部だけ切る録画だけを投入する。
func TestEncodeReconcile_AllCutRecordingEnqueuesNothing(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	seed := func(rel string, spans ...chapters.Span) int64 {
		id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, rel, []string{"cut"}, []byte("x"))
		testutil.MustExec(t, pool, "setting program duration", `UPDATE recordings SET program_duration_ms = 1800000 WHERE id = $1`, id)
		dbtest.AdoptChapters(t, pool, id)
		for _, s := range spans {
			dbtest.ChapterSpan(t, pool, id, func(p *dbtest.ChapterSpanParams) {
				p.StartMs, p.EndMs = s.StartMs, s.EndMs
			})
		}
		return id
	}
	allCut := seed("cut/allcut.m2ts", chapters.Span{StartMs: 0, EndMs: 1800000})
	partial := seed("cut/partial.m2ts", chapters.Span{StartMs: 500, EndMs: 1000})

	cfg := config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}}
	enqueuer := runEncodeReconcilePass(t, &EncodeReconcileWorker{Profiles: cfg, Media: pgstoretest.Media(pool)})
	if got := pendingEncodeProfiles(t, enqueuer, allCut); len(got) != 0 {
		t.Errorf("reconcile path: all-cut recording pending = %v, want none", got)
	}
	if got := pendingEncodeProfiles(t, enqueuer, partial); !slices.Equal(got, []string{"cut"}) {
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
	t.Parallel()
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
	testutil.RequireNoError(t, err, "CutFilterComplex")
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
	testutil.RequireNoError(t, err, "decoding audio")
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
