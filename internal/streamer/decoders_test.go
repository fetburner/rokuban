package streamer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/testexec"
)

// writeFakeFFmpegDecoders は `ffmpeg -hide_banner -decoders` の出力を返す偽 ffmpeg を作る。
// calls を渡すと、実行されるたびに 1 行追記する（実行回数を数えるため）。
func writeFakeFFmpegDecoders(t *testing.T, decoderLines string, calls string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	script := "#!/bin/sh\n"
	if calls != "" {
		script += "echo x >> " + calls + "\n"
	}
	script += "cat <<'EOF'\n" + decoderLines + "EOF\n"
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	testexec.WriteExecutable(t, path, []byte(script))
	return path
}

const (
	mpeg2Line = " V....D mpeg2video           MPEG-2 video\n"
	aribLine  = " S..... libaribcaption        ARIB caption decoder\n"
	h264Line  = " V....D h264                  H.264 / AVC\n"
)

// TestValidateFFmpegDecoders は live / 録画 HLS の起動時検査の条件を固定する。
// mpeg2video は常に要る。libaribcaption は captions が true のときだけ要る。
// 壊し方: mpeg2 の判定を消す（1 つめが落ちる）、captions の判定を消す（3 つめが落ちる）、
// 2 回目の `ffmpeg -decoders` 実行を足す（最後のサブテストが落ちる）。
func TestValidateFFmpegDecoders(t *testing.T) {
	ctx := context.Background()

	t.Run("mpeg2 missing errors even without captions", func(t *testing.T) {
		ffmpeg := writeFakeFFmpegDecoders(t, h264Line+aribLine, "")
		err := ValidateFFmpegDecoders(ctx, ffmpeg, false)
		if err == nil || !strings.Contains(err.Error(), "mpeg2video") {
			t.Fatalf("ValidateFFmpegDecoders() = %v, want missing mpeg2video", err)
		}
	})

	t.Run("mpeg2 present passes without captions", func(t *testing.T) {
		ffmpeg := writeFakeFFmpegDecoders(t, mpeg2Line, "")
		if err := ValidateFFmpegDecoders(ctx, ffmpeg, false); err != nil {
			t.Fatalf("ValidateFFmpegDecoders() = %v, want nil", err)
		}
	})

	t.Run("captions and libaribcaption missing", func(t *testing.T) {
		ffmpeg := writeFakeFFmpegDecoders(t, mpeg2Line, "")
		err := ValidateFFmpegDecoders(ctx, ffmpeg, true)
		if err == nil || !strings.Contains(err.Error(), "libaribcaption") {
			t.Fatalf("ValidateFFmpegDecoders() = %v, want missing libaribcaption", err)
		}
	})

	t.Run("captions and both decoders present passes", func(t *testing.T) {
		ffmpeg := writeFakeFFmpegDecoders(t, mpeg2Line+aribLine, "")
		if err := ValidateFFmpegDecoders(ctx, ffmpeg, true); err != nil {
			t.Fatalf("ValidateFFmpegDecoders() = %v, want nil", err)
		}
	})

	// captions のときも ffmpeg -decoders は 1 回だけ実行する。
	t.Run("captions lists decoders once", func(t *testing.T) {
		calls := filepath.Join(t.TempDir(), "calls")
		ffmpeg := writeFakeFFmpegDecoders(t, mpeg2Line+aribLine, calls)
		if err := ValidateFFmpegDecoders(ctx, ffmpeg, true); err != nil {
			t.Fatalf("ValidateFFmpegDecoders() = %v, want nil", err)
		}
		got, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(got), "x"); n != 1 {
			t.Fatalf("ffmpeg -decoders executed %d times, want 1", n)
		}
	})
}
