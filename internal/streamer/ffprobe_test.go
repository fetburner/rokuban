package streamer

import (
	"os"
	"os/exec"
	"testing"
)

// lookPathFFprobe は lookPathFFmpeg の ffprobe 版（ROKUBAN_REQUIRE_FFMPEG で skip を禁じる）。
func lookPathFFprobe(t *testing.T) string {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("ROKUBAN_REQUIRE_FFMPEG") != "" {
			t.Fatalf("ffprobe not in PATH but ROKUBAN_REQUIRE_FFMPEG is set: %v", err)
		}
		t.Skip("ffprobe not in PATH")
	}
	return ffprobe
}
