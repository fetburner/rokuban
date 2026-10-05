package ffargs

import (
	"slices"
	"testing"
)

func TestProbeArgumentBuilders(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "default stream selection",
			got:  DefaultStreamSelectionProbeArgs("/input.ts"),
			want: []string{"-v", "error", "-show_entries", "stream=index,codec_type,width,height,channels", "-of", "csv=p=0", "/input.ts"},
		},
		{
			name: "subtitle file",
			got:  SubtitleProbeArgs([]string{"/input.ts"}, "", ""),
			want: []string{"-v", "error", "-select_streams", "s", "-show_entries", "stream=index", "-of", "csv=p=0", "/input.ts"},
		},
		{
			name: "subtitle live input with probe limits",
			got:  SubtitleProbeArgs([]string{"-i", "pipe:0"}, "5M", "3M"),
			want: []string{"-v", "error", "-probesize", "5M", "-analyzeduration", "3M", "-select_streams", "s", "-show_entries", "stream=index", "-of", "csv=p=0", "-i", "pipe:0"},
		},
		{
			name: "thumbnail format duration",
			got:  ThumbnailDurationProbeArgs("/input.ts"),
			want: []string{"-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", "/input.ts"},
		},
		{
			name: "video stream duration",
			got:  VideoStreamDurationProbeArgs("/input.ts"),
			want: []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "stream=duration", "-of", "default=noprint_wrappers=1:nokey=1", "/input.ts"},
		},
		{
			name: "original VOD timestamps",
			got:  OriginalVODDurationProbeArgs("/dev/fd/3"),
			want: []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "format=start_time,duration:stream=start_time,duration", "-of", "json", "-i", "/dev/fd/3"},
		},
		{
			name: "format start time",
			got:  FormatStartTimeProbeArgs("/input.ts"),
			want: []string{"-v", "error", "-show_entries", "format=start_time", "-of", "default=noprint_wrappers=1:nokey=1", "/input.ts"},
		},
		{
			name: "frame properties",
			got:  FrameProbeArgs("/input.ts", "9.500%+3.500"),
			want: []string{"-v", "error", "-select_streams", "v:0", "-read_intervals", "9.500%+3.500", "-show_entries", "frame=best_effort_timestamp_time,width,height,sample_aspect_ratio", "-of", "csv=p=0", "/input.ts"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Equal(tt.got, tt.want) {
				t.Fatalf("args = %#v, want %#v", tt.got, tt.want)
			}
		})
	}
}

func TestToolPathFallbacks(t *testing.T) {
	for _, tt := range []struct {
		name      string
		got, want string
	}{
		{name: "ffmpeg default", got: FFmpegPath(""), want: "ffmpeg"},
		{name: "ffmpeg configured", got: FFmpegPath("/tools/ffmpeg"), want: "/tools/ffmpeg"},
		{name: "ffprobe default", got: FFprobePath(""), want: "ffprobe"},
		{name: "ffprobe configured", got: FFprobePath("/tools/ffprobe"), want: "/tools/ffprobe"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("path = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestSquarePixelsFilter(t *testing.T) {
	if SquarePixelsFilter != "scale=round(iw*sar/2)*2:ih,setsar=1" {
		t.Fatalf("filter = %q, want square pixel normalization filter", SquarePixelsFilter)
	}
}
