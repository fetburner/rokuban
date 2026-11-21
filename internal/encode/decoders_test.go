package encode

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/testexec"
)

// writeFakeFFmpegDecoders は `ffmpeg -decoders` の代わりに 1 行だけ返す偽 ffmpeg を作る。
func writeFakeFFmpegDecoders(t *testing.T, includeARIB bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	line := " V..... mpeg4                 MPEG-4 part 2"
	if includeARIB {
		line = " S..... libaribcaption        ARIB caption decoder"
	}
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	testexec.WriteExecutable(t, path, []byte("#!/bin/sh\necho \""+line+"\"\n"))
	return path
}

// TestValidateDecoders_LibARIBCaption は subtitles: webvtt のプロファイルがあるときだけ、
// encode.ffmpeg に libaribcaption が要ることを固定する。
// 呼び出し自体（server.go から消すこと）はこのテストでは見えない。それは cmd/rokuban の
// TestServerCmd_FFmpegDecoderCheckFailsStartup が見る。
// 壊し方: webvtt の条件を外す（字幕を使わない構成でも落ちる。3 つめのサブテストが落ちる）、
// ValidateLibARIBCaption の判定を反転する（1 つめが落ちる）。
func TestValidateDecoders_LibARIBCaption(t *testing.T) {
	t.Run("errors when a webvtt profile has no libaribcaption", func(t *testing.T) {
		cfg := config.EncodeConfig{
			FFmpeg:   writeFakeFFmpegDecoders(t, false),
			Profiles: []config.EncodeProfile{{Name: "web", Subtitles: "webvtt"}},
		}
		err := ValidateDecoders(context.Background(), cfg)
		if err == nil {
			t.Fatal("ValidateDecoders() = nil, want libaribcaption error")
		}
		if !strings.Contains(err.Error(), "libaribcaption") {
			t.Errorf("error = %v, want mention of libaribcaption", err)
		}
	})

	t.Run("passes when a webvtt profile has libaribcaption", func(t *testing.T) {
		cfg := config.EncodeConfig{
			FFmpeg:   writeFakeFFmpegDecoders(t, true),
			Profiles: []config.EncodeProfile{{Name: "web", Subtitles: "webvtt"}},
		}
		if err := ValidateDecoders(context.Background(), cfg); err != nil {
			t.Fatalf("ValidateDecoders() = %v, want nil", err)
		}
	})

	t.Run("no webvtt profile skips the check even without libaribcaption", func(t *testing.T) {
		cfg := config.EncodeConfig{
			// 存在しない ffmpeg。検査が走れば exec の失敗で落ちるので、走らないことも見える。
			FFmpeg:   "rokuban-no-such-ffmpeg-binary",
			Profiles: []config.EncodeProfile{{Name: "h264"}},
		}
		if err := ValidateDecoders(context.Background(), cfg); err != nil {
			t.Fatalf("ValidateDecoders() = %v, want nil without any webvtt profile", err)
		}
	})
}
