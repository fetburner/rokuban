package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/streamer"
)

const playbackTimelineOffsetSeconds = int64(10)

var playbackTimelineMarkerFrames = []int{45, 106, 181, 240, 330, 401, 492, 540, 624}

type playbackTimelineFixtureManifest struct {
	Source        string `json:"source"`
	Encoded       string `json:"encoded"`
	OffsetSeconds int64  `json:"offsetSeconds"`
	MarkerFrames  []int  `json:"markerFrames"`
	HLS           struct {
		Offset0  string `json:"offset0"`
		Offset10 string `json:"offset10"`
	} `json:"hls"`
}

// TestWritePlaybackTimelineFixture exports source and product-encoded artifacts for
// web/e2e/recording-playback-timeline.mjs when ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR
// is set. Go builds these outputs so the browser measurement follows the same argv
// builders as the streamer and worker; a JavaScript-only ffmpeg command could drift
// from product behavior while still making the timeline assertion pass.
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

	sourcePath := filepath.Join(fixtureDir, "original.ts")
	filterParts := make([]string, 0, len(playbackTimelineMarkerFrames))
	for _, frame := range playbackTimelineMarkerFrames {
		filterParts = append(filterParts, fmt.Sprintf("eq(n\\,%d)", frame))
	}
	videoFilter := "drawbox=x=iw/2-24:y=ih/2-24:w=48:h=48:color=white:t=fill:enable='" + strings.Join(filterParts, "+") + "'"
	videoInput := "color=c=black:s=320x180:r=30000/1001:d=22"
	audioInput := "sine=frequency=440:sample_rate=48000:duration=22.7"
	inputArgs := []string{
		"-hide_banner", "-nostats", "-loglevel", "error", "-y",
		"-itsoffset", "0.7", "-f", "lavfi", "-i", videoInput,
		"-f", "lavfi", "-i", audioInput,
		"-map", "0:v:0", "-map", "1:a:0", "-vf", videoFilter,
		"-output_ts_offset", "9",
		"-c:v", "mpeg2video", "-b:v", "500k", "-g", "60", "-bf", "2",
		"-flags", "+ilme+ildct", "-pix_fmt", "yuv420p",
		"-c:a", "mp2", "-b:a", "128k", "-f", "mpegts", sourcePath,
	}
	runPlaybackTimelineFFmpeg(t, ffmpeg, inputArgs, nil)

	liveConfig := streamer.LiveConfig{
		FFmpeg: ffmpeg,
		Profiles: []streamer.LiveProfile{{
			Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", Height: 720,
			Scaler: ffargs.ScalerSoftware, Deinterlace: true, Preset: "veryfast",
			SegmentSeconds: 2, PlaylistSize: 6,
		}},
	}
	for _, offset := range []int64{0, playbackTimelineOffsetSeconds} {
		dir := filepath.Join(fixtureDir, fmt.Sprintf("hls-offset-%d", offset))
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
		MarkerFrames: append([]int(nil), playbackTimelineMarkerFrames...),
	}
	manifest.HLS.Offset0 = "hls-offset-0/hd.m3u8"
	manifest.HLS.Offset10 = fmt.Sprintf("hls-offset-%d/hd.m3u8", playbackTimelineOffsetSeconds)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "manifest.json"), append(manifestBytes, '\n'), 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
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
