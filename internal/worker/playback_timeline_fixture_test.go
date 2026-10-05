package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/streamer"
)

const playbackTimelineOffsetSeconds = int64(10)

var playbackTimelineMarkerFrames = []int{45, 106, 181, 240, 330, 401, 492, 540, 624}

type playbackTimelineFixtureManifest struct {
	Source        string                                       `json:"source"`
	Encoded       string                                       `json:"encoded"`
	OffsetSeconds int64                                        `json:"offsetSeconds"`
	MarkerFrames  []int                                        `json:"markerFrames"`
	HLSVariants   map[string]playbackTimelineFixtureHLSVariant `json:"hlsVariants"`
	HLS           struct {
		Offset0  string `json:"offset0"`
		Offset10 string `json:"offset10"`
	} `json:"hls"`
}

type playbackTimelineFixtureHLSVariant struct {
	Source      string  `json:"source"`
	Playlist    string  `json:"playlist"`
	GOPFrames   int     `json:"gopFrames"`
	AudioOffset float64 `json:"audioOffsetSeconds"`
}

// TestWritePlaybackTimelineFixture は、環境変数 ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR が
// 設定されているとき、web/e2e/recording-playback-timeline.mjs 用の原本 TS と、製品の
// 引数ビルダーで作った HLS・非カット MP4 をそのディレクトリへ書き出す。出力を Go 側で
// 作るのは、streamer / worker と同じ引数ビルダーを通すためである。JavaScript 側で
// ffmpeg の引数を組むと、製品の挙動からずれたまま時間軸の判定だけ通りうる。
func TestWritePlaybackTimelineFixture(t *testing.T) {
	fixtureDir := os.Getenv("ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR")
	if fixtureDir == "" {
		t.Skip("set ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR to export browser timeline fixtures")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("ffmpeg is required to export playback timeline fixtures: %v", err)
	}
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}

	filterParts := make([]string, 0, len(playbackTimelineMarkerFrames))
	markerPositions := [][2]string{
		{"iw/4-24", "ih/4-24"}, {"iw/2-24", "ih/4-24"}, {"iw*3/4-24", "ih/4-24"},
		{"iw/4-24", "ih/2-24"}, {"iw/2-24", "ih/2-24"}, {"iw*3/4-24", "ih/2-24"},
		{"iw/4-24", "ih*3/4-24"}, {"iw/2-24", "ih*3/4-24"}, {"iw*3/4-24", "ih*3/4-24"},
	}
	for slot, frame := range playbackTimelineMarkerFrames {
		position := markerPositions[slot]
		filterParts = append(filterParts, fmt.Sprintf(
			"drawbox=x=%s:y=%s:w=48:h=48:color=white:t=fill:enable='eq(n\\,%d)'",
			position[0], position[1], frame,
		))
	}
	// The marker slot is encoded by its position in a 3x3 grid. The browser can
	// decode the actual displayed frame number instead of trusting the requested one.
	videoFilter := strings.Join(filterParts, ",")
	videoInput := "color=c=black:s=320x180:r=30000/1001:d=22"
	audioInput := "sine=frequency=440:sample_rate=48000:duration=22.7"
	// The audio input is delayed by 16.7 ms (audio start 10.4067 s) so that one source
	// frame's -ss cutoff falls between the integer -ss 10 and the frame-grid
	// -ss 9.976633333: -ss 10 then selects a different first frame than the grid
	// value (measured with ffmpeg framemd5). Without this delay both selected the
	// same first frame and an integer -ss was not caught.
	writeSource := func(fileName string, gopFrames int, audioOffsetSeconds float64) string {
		sourcePath := filepath.Join(fixtureDir, fileName)
		inputArgs := []string{
			"-hide_banner", "-nostats", "-loglevel", "error", "-y",
			"-itsoffset", "0.7", "-f", "lavfi", "-i", videoInput,
			"-itsoffset", strconv.FormatFloat(audioOffsetSeconds, 'f', -1, 64), "-f", "lavfi", "-i", audioInput,
			"-map", "0:v:0", "-map", "1:a:0", "-vf", videoFilter,
			"-output_ts_offset", "9",
			"-c:v", "mpeg2video", "-b:v", "500k", "-g", strconv.Itoa(gopFrames), "-bf", "2",
			"-flags", "+ilme+ildct", "-pix_fmt", "yuv420p",
			"-c:a", "mp2", "-b:a", "128k", "-f", "mpegts", sourcePath,
		}
		runPlaybackTimelineFFmpeg(t, ffmpeg, inputArgs, nil)
		return sourcePath
	}
	sourcePath := writeSource("original.ts", 60, 0.0167)

	liveConfig := streamer.LiveConfig{
		FFmpeg: ffmpeg,
		Profiles: []streamer.LiveProfile{{
			Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", Height: 720,
			Scaler: ffargs.ScalerSoftware, Deinterlace: true, Preset: "veryfast",
			SegmentSeconds: 2, PlaylistSize: 6,
		}},
	}
	writeHLS := func(sourcePath, name string, offset int64) string {
		dir := filepath.Join(fixtureDir, fmt.Sprintf("hls-offset-%d", offset))
		if name != "" {
			dir = filepath.Join(fixtureDir, name)
		}
		if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
			t.Fatalf("create HLS output directory: %v", err)
		}
		source, err := os.Open(sourcePath)
		if err != nil {
			t.Fatalf("open source TS for offset %d: %v", offset, err)
		}
		args := streamer.BuildOriginalVODFFmpegArgs(liveConfig, dir, false, offset)
		runPlaybackTimelineFFmpeg(t, ffmpeg, args, []*os.File{source})
		if err := source.Close(); err != nil {
			t.Fatalf("close source TS after offset %d: %v", offset, err)
		}
		return filepath.Join(dir, "hd.m3u8")
	}
	baselineOffset0 := ""
	baselineOffset10 := ""
	for _, offset := range []int64{0, playbackTimelineOffsetSeconds} {
		playlist := writeHLS(sourcePath, "", offset)
		if offset == 0 {
			baselineOffset0 = playlist
		} else {
			baselineOffset10 = playlist
		}
	}
	variants := map[string]playbackTimelineFixtureHLSVariant{}
	for _, variant := range []struct {
		name               string
		dir                string
		gopFrames          int
		audioOffsetSeconds float64
	}{
		{name: "shortGOP", dir: "hls-short-gop", gopFrames: 12, audioOffsetSeconds: 0.0167},
		{name: "audioAligned", dir: "hls-audio-aligned", gopFrames: 60, audioOffsetSeconds: 0.7},
	} {
		sourceName := variant.dir + ".ts"
		variantSource := writeSource(sourceName, variant.gopFrames, variant.audioOffsetSeconds)
		playlist := writeHLS(variantSource, variant.dir, 0)
		variants[variant.name] = playbackTimelineFixtureHLSVariant{
			Source: sourceName, Playlist: filepath.ToSlash(mustRelativePath(t, fixtureDir, playlist)),
			GOPFrames: variant.gopFrames, AudioOffset: variant.audioOffsetSeconds,
		}
	}

	// These are the h264 example settings in config.example.yml: progressive MP4,
	// H.264/AAC, 1080p, software deinterlace, CRF 23, medium preset.
	crf := 23
	profile := config.EncodeProfile{
		Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		Height: 1080, Deinterlace: true, CRF: &crf, Preset: "medium",
	}
	encodedPath := filepath.Join(fixtureDir, "encoded.mp4")
	encodedArgs := BuildFFmpegArgs(profile, sourcePath, encodedPath, false, nil)
	runPlaybackTimelineFFmpeg(t, ffmpeg, encodedArgs, nil)

	manifest := playbackTimelineFixtureManifest{
		Source: "original.ts", Encoded: "encoded.mp4", OffsetSeconds: playbackTimelineOffsetSeconds,
		MarkerFrames: append([]int(nil), playbackTimelineMarkerFrames...), HLSVariants: variants,
	}
	manifest.HLS.Offset0 = filepath.ToSlash(mustRelativePath(t, fixtureDir, baselineOffset0))
	manifest.HLS.Offset10 = filepath.ToSlash(mustRelativePath(t, fixtureDir, baselineOffset10))
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "manifest.json"), append(manifestBytes, '\n'), 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
}

func mustRelativePath(t *testing.T, base, path string) string {
	t.Helper()
	relative, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatalf("make fixture path relative: %v", err)
	}
	return relative
}

func runPlaybackTimelineFFmpeg(t *testing.T, ffmpeg string, args []string, extraFiles []*os.File) {
	t.Helper()
	cmd := exec.Command(ffmpeg, args...)
	cmd.ExtraFiles = extraFiles
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, output)
	}
}
