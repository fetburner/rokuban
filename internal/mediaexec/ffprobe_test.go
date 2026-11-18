package mediaexec

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProbeVideoDuration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   time.Duration
		err    string
	}{
		{name: "first value", output: " 12.5\n45\n", want: 12500 * time.Millisecond},
		{name: "unavailable", output: "N/A\n45\n", err: "ffprobe returned empty video duration"},
		{name: "invalid", output: "invalid\n45\n", err: "parsing video duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				wantArgs := []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "stream=duration", "-of", "default=noprint_wrappers=1:nokey=1", "input.ts"}
				if name != "ffprobe" || !slices.Equal(args, wantArgs) {
					t.Fatalf("probe command = %s %v, want ffprobe %v", name, args, wantArgs)
				}
				return []byte(tc.output), nil
			}
			got, err := ProbeVideoDuration(context.Background(), run, "", "input.ts")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("duration = %s, error = %v, want %s", got, err, tc.want)
			}
		})
	}
}
