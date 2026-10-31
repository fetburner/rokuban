package streamer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

const (
	segmentedFixtureSeconds = 60
	segmentedFixtureBits    = 11
	// segmentedFixtureRestartNumber は再起動の run の startNumber。境界は次の格子点（番号 6 = 12 秒）で、
	// 製品が要求の 1 つ前から変換を始める（startNumber = 要求 - 1）形に合わせてある。
	segmentedFixtureRestartNumber      = int64(5)
	segmentedFixtureWorstRestartNumber = int64(16)
	// segmentedFixtureOffGridSeconds は off-grid の負例が読み始める位置を、格子点から後ろへずらす秒数。
	segmentedFixtureOffGridSeconds = 1
)

type segmentedFixtureRun struct {
	Directory   string `json:"directory"`
	StartNumber int64  `json:"startNumber"`
}

type segmentedFixtureManifest struct {
	Source         string                         `json:"source"`
	OriginPTS      int64                          `json:"originPTS"`
	FrameRate      string                         `json:"frameRate"`
	FixtureSeconds int                            `json:"fixtureSeconds"`
	SegmentSeconds int                            `json:"segmentSeconds"`
	FrameBits      int                            `json:"frameBits"`
	RestartNumber  int64                          `json:"restartNumber"`
	Runs           map[string]segmentedFixtureRun `json:"runs"`
	Codecs         string                         `json:"codecs"`
	ProfileName    string                         `json:"profileName"`
}

// TestWriteSegmentedPlaybackFixture は、環境変数 ROKUBAN_SEGMENTED_PLAYBACK_FIXTURE_DIR が設定されて
// いるとき、web/e2e/recording-segmented-vod.mjs 用の 29.97 fps の合成 TS と、製品の
// buildSegmentTransformArgs で変換した run を書き出す。run は連続 1 本（startNumber 0）、再起動
// （startNumber 5）と、その再起動の引数を 1 か所だけ変えた負例 3 つである。playlist の合成は JS 側が
// 担う。ROKUBAN_SEGMENTED_PLAYBACK_FMP4=1 のときは、コンテナだけを替えた比較用の run も作る。
// JavaScript で ffmpeg の HLS 引数を組むと、製品の引数と時間軸の扱いが食い違ったまま
// 判定だけ通りうる（先頭フレームの複製と第 1 run の時間軸のずれが、実際に正例へ入っていた）。
func TestWriteSegmentedPlaybackFixture(t *testing.T) {
	fixtureDir := os.Getenv("ROKUBAN_SEGMENTED_PLAYBACK_FIXTURE_DIR")
	if fixtureDir == "" {
		t.Skip("set ROKUBAN_SEGMENTED_PLAYBACK_FIXTURE_DIR to export segmented playback fixtures")
	}
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}

	sourcePath := filepath.Join(fixtureDir, "source.ts")
	writeSegmentedFixtureSource(t, ffmpeg, sourcePath)
	originPTS := firstPacketPTS(t, ffprobe, sourcePath)

	profile := LiveProfile{
		Name: "hd", VideoCodec: "libx264", AudioCodec: "aac",
		Preset: "ultrafast", SegmentSeconds: 2, PlaylistSize: 0,
	}
	cfg := LiveConfig{FFmpeg: ffmpeg}
	// 製品の原本と同じく、長さが確定した入力にする（音声に apad,atrim=end が付く）。
	input := SegmentTransformInput{
		Kind: store.RecordingHLSInputOriginal, OriginPTS: originPTS, AudioStreamCount: 1,
	}
	input.VideoDurationPTS, input.VideoLastFramePTS = videoSpanPTS(t, ffprobe, sourcePath)

	runs := map[string]segmentedFixtureRun{}
	runFormat := func(name string, startNumber int64, mutate func(args []string) []string, fmp4 bool) {
		if fmp4 {
			name = "fmp4-" + name
		}
		t.Helper()
		directory := filepath.Join(fixtureDir, "runs", name)
		if err := os.RemoveAll(directory); err != nil {
			t.Fatalf("clear %s: %v", directory, err)
		}
		if err := os.MkdirAll(filepath.Join(directory, "segments"), 0o755); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		args, err := buildSegmentTransformArgs(cfg, profile, input, startNumber, directory)
		if err != nil {
			t.Fatalf("%s: build args: %v", name, err)
		}
		if fmp4 {
			// 比較用は製品の変換引数を保ち、HLS のコンテナとファイル名だけを替える。
			index := indexOf(args, "-hls_segment_filename")
			if index < 0 || index+1 >= len(args) {
				t.Fatalf("%s: no segment filename", name)
			}
			args[index+1] = strings.TrimSuffix(args[index+1], ".ts") + ".m4s"
			args = append(args[:len(args)-1], append([]string{
				"-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init_%v.mp4",
				// fMP4 の tfdt は既定でその run の先頭を 0 にし、-output_ts_offset を含まない。再起動の run が
				// 起動位置からの時刻（12 秒の境界で 2.01 秒）になったので、frag_discont で元の時刻を書かせる。
				"-hls_segment_options", "movflags=+frag_discont",
			}, args[len(args)-1:]...)...)
		}
		if mutate != nil {
			args = mutate(args)
		}
		// 製品と同じく、原本は fd 3 として渡す。
		file, err := os.Open(sourcePath)
		if err != nil {
			t.Fatalf("%s: open source: %v", name, err)
		}
		defer func() { _ = file.Close() }()
		cmd := exec.Command(ffmpeg, args...)
		cmd.ExtraFiles = []*os.File{file}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg %v: %v\n%s", name, args, err, output)
		}
		runs[name] = segmentedFixtureRun{Directory: filepath.Join("runs", name), StartNumber: startNumber}
	}

	run := func(name string, startNumber int64, mutate func(args []string) []string) {
		runFormat(name, startNumber, mutate, false)
		if os.Getenv("ROKUBAN_SEGMENTED_PLAYBACK_FMP4") == "1" {
			runFormat(name, startNumber, mutate, true)
		}
	}

	run("direct", 0, nil)
	run("restart", segmentedFixtureRestartNumber, nil)
	// 29.97 fps では再起動の位置によって先頭フレームの格子からのずれが変わる。番号 16 は最大に近い（約 32 ms 早い）。
	run("restart-k16", segmentedFixtureWorstRestartNumber, nil)

	// 負例は、製品の引数から 1 か所だけ変えたもの。
	gridSeconds := segmentedFixtureRestartNumber * int64(profile.SegmentSeconds)
	run("off-grid", segmentedFixtureRestartNumber, func(args []string) []string {
		return replaceSegmentedFixturePosition(t, args, originPTS, gridSeconds, gridSeconds+segmentedFixtureOffGridSeconds)
	})
	run("no-offset", segmentedFixtureRestartNumber, func(args []string) []string {
		return dropSegmentedFixtureOutputTSOffset(t, args)
	})
	run("audio-gap", segmentedFixtureRestartNumber, func(args []string) []string {
		// 再起動の run の 2 秒後（= 境界。run の先頭の segment は捨てる）から、AAC 1 フレーム分
		// （1024 samples）の無音を標準音声へ入れる。
		index := indexOf(args, "-filter:a:0")
		if index < 0 || index+1 >= len(args) {
			t.Fatalf("segment args have no -filter:a:0: %v", args)
		}
		args = append([]string(nil), args...)
		args[index+1] += fmt.Sprintf(",volume=0:enable='between(t,2,%.9f)'", 2+1024.0/48000)
		return args
	})

	masterBytes, err := os.ReadFile(filepath.Join(fixtureDir, "runs", "direct", "hd.m3u8"))
	if err != nil {
		t.Fatalf("read ffmpeg master: %v", err)
	}
	codecs := ""
	for _, line := range strings.Split(string(masterBytes), "\n") {
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			if _, rest, ok := strings.Cut(line, `CODECS="`); ok {
				codecs, _, _ = strings.Cut(rest, `"`)
			}
		}
	}
	if codecs == "" {
		t.Fatalf("no CODECS in ffmpeg master: %s", masterBytes)
	}

	manifest := segmentedFixtureManifest{
		Source: "source.ts", OriginPTS: originPTS, FrameRate: "30000/1001",
		FixtureSeconds: segmentedFixtureSeconds, SegmentSeconds: profile.SegmentSeconds,
		FrameBits: segmentedFixtureBits, RestartNumber: segmentedFixtureRestartNumber,
		Runs: runs, Codecs: codecs, ProfileName: profile.Name,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// dropSegmentedFixtureOutputTSOffset は、引数から -output_ts_offset とその値を外す。
func dropSegmentedFixtureOutputTSOffset(t *testing.T, args []string) []string {
	t.Helper()
	index := indexOf(args, "-output_ts_offset")
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("segment args have no -output_ts_offset: %v", args)
	}
	return append(append([]string(nil), args[:index]...), args[index+2:]...)
}

// replaceSegmentedFixturePosition は、fromSeconds の格子点に向けて組まれた引数の seek / trim /
// output_ts_offset の位置を toSeconds へ書き換える。製品の startNumber は格子点しか表せないので、
// 格子点ではない位置から起動した引数は、組んだ引数の値を置き換えて作る。
func replaceSegmentedFixturePosition(t *testing.T, args []string, originPTS, fromSeconds, toSeconds int64) []string {
	t.Helper()
	format := func(pts int64) string {
		return strconv.FormatFloat(float64(pts)/float64(mpegTSTimestampClock), 'f', 9, 64)
	}
	preRoll := int64(2) * mpegTSTimestampClock
	pairs := [][2]string{
		{format(originPTS + fromSeconds*mpegTSTimestampClock), format(originPTS + toSeconds*mpegTSTimestampClock)},
		{format(originPTS + fromSeconds*mpegTSTimestampClock - preRoll), format(originPTS + toSeconds*mpegTSTimestampClock - preRoll)},
	}
	out := make([]string, len(args))
	replaced := 0
	for i, arg := range args {
		out[i] = arg
		for _, pair := range pairs {
			if strings.Contains(out[i], pair[0]) {
				out[i] = strings.ReplaceAll(out[i], pair[0], pair[1])
				replaced++
			}
		}
	}
	// -ss 1 つ、-output_ts_offset 1 つ、映像の trim 1 つ、音声の atrim 3 つ。
	if replaced != 6 {
		t.Fatalf("replaced %d position values in %v, want 6", replaced, args)
	}
	return out
}

// writeSegmentedFixtureSource は、29.97 fps（30000/1001）の H.264 + 2ch AAC（左 440 Hz、右 880 Hz）の
// TS を書く。映像にはフレーム番号を 11 bit の箱で焼き込む。29.97 fps にするのは、実際の録画と同じく
// 2 秒の格子にフレームが割り切れない（59.94 フレーム）条件を入れるためである。
func writeSegmentedFixtureSource(t *testing.T, ffmpeg, path string) {
	t.Helper()
	filters := []string{"drawbox=x=16:y=8:w=496:h=64:color=black:t=fill"}
	for bit := 0; bit < segmentedFixtureBits; bit++ {
		filters = append(filters, fmt.Sprintf(
			"drawbox=x=%d:y=18:w=28:h=44:color=white:t=fill:enable='gt(mod(floor(n/%d),2),0)'",
			28+bit*40, 1<<bit))
	}
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30000/1001",
		"-f", "lavfi", "-i", fmt.Sprintf("aevalsrc=0.5*sin(2*PI*440*t)|0.5*sin(2*PI*880*t):s=48000:d=%d", segmentedFixtureSeconds),
		"-t", strconv.Itoa(segmentedFixtureSeconds),
		"-map", "0:v:0", "-map", "1:a:0", "-vf", strings.Join(filters, ","),
		"-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "baseline", "-level", "3.0",
		"-pix_fmt", "yuv420p", "-g", "60", "-keyint_min", "60", "-sc_threshold", "0", "-bf", "0",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-f", "mpegts", path,
	}
	if output, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("generating source TS: %v\n%s", err, output)
	}
}
