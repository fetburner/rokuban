package config

/* Shared FFmpeg tool and decoder validation. */

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ffmpegDecoders は `ffmpeg -decoders` の出力を返す。
func ffmpegDecoders(ffmpeg string) ([]byte, error) {
	out, err := exec.Command(ffmpeg, "-hide_banner", "-decoders").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("listing decoders of ffmpeg %q: %w", ffmpeg, err)
	}
	return out, nil
}

// validateFFmpegDecoder checks a named input decoder in the `ffmpeg -decoders`
// output before the live streamer starts accepting requests. Live HLS consumes
// MPEG-2 TS originals; without this decoder every generated HLS profile would
// fail only after a viewer asks for playback and leave an empty player.
func validateFFmpegDecoder(decoders []byte, decoder, scope string) error {
	if !regexp.MustCompile(`(?m)^\s*V\S*\s+` + regexp.QuoteMeta(decoder) + `\s`).Match(decoders) {
		return fmt.Errorf("%s requires an ffmpeg build with %q decoder", scope, decoder)
	}
	return nil
}

// validateLibARIBCaption は字幕を有効にした構成で、実際に使う ffmpeg が
// libaribcaption デコーダを持つこと（`ffmpeg -decoders` の出力 decoders）を起動時に
// 検査する。Debian bookworm の apt 版 ffmpeg 5.1 には通常含まれないため、
// 設定したのに字幕だけ黙って消える状態を許さない。
func validateLibARIBCaption(decoders []byte, scope string) error {
	if !strings.Contains(string(decoders), "libaribcaption") {
		return fmt.Errorf("%s subtitles require an ffmpeg build with libaribcaption decoder", scope)
	}
	return nil
}
