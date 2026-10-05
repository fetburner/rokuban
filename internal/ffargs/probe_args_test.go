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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Equal(tt.got, tt.want) {
				t.Fatalf("args = %#v, want %#v", tt.got, tt.want)
			}
		})
	}
}
