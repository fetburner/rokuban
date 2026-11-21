package mediaexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/testexec"
)

// TestValidateLibARIBCaption は字幕用デコーダの判定を固定する。
// 壊し方: strings.Contains の判定を反転する、または ValidateLibARIBCaption が
// 常に nil を返すようにすると、対応する側のサブテストが落ちる。
func TestValidateLibARIBCaption(t *testing.T) {
	t.Run("errors without libaribcaption", func(t *testing.T) {
		err := ValidateLibARIBCaption([]byte(" V..... mpeg4  MPEG-4 part 2\n"), "encode")
		if err == nil {
			t.Fatal("ValidateLibARIBCaption() = nil, want error without libaribcaption")
		}
		if !strings.Contains(err.Error(), "encode") || !strings.Contains(err.Error(), "libaribcaption") {
			t.Errorf("error = %v, want scope and libaribcaption", err)
		}
	})

	t.Run("passes with libaribcaption", func(t *testing.T) {
		decoders := []byte(" S..... libaribcaption        ARIB caption decoder\n")
		if err := ValidateLibARIBCaption(decoders, "live"); err != nil {
			t.Fatalf("ValidateLibARIBCaption() = %v, want nil", err)
		}
	})
}

// TestFFmpegDecodersHonorsContext は、キャンセル済みの ctx では ffmpeg を起動せず、
// ctx のエラーを返すことを固定する（呼び出し元の ctx を通す契約）。
// 偽 ffmpeg は起動されると印を残す。
// 壊し方: FFmpegDecoders の CommandOutput に渡す ctx を context.Background() に置き換えると、
// エラーが返らず、印も残るので落ちる。
func TestFFmpegDecodersHonorsContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	ffmpeg := filepath.Join(dir, "fake-ffmpeg")
	testexec.WriteExecutable(t, ffmpeg, []byte("#!/bin/sh\ntouch "+marker+"\necho ' V..... mpeg2video'\n"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FFmpegDecoders(ctx, ffmpeg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FFmpegDecoders() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("FFmpegDecoders started ffmpeg despite a canceled context")
	}
}
