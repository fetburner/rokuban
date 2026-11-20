package mediaexec

import (
	"context"
	"fmt"
	"strings"
)

// FFmpegDecoders は `ffmpeg -hide_banner -decoders` の出力を返す。
// 起動時にデコーダの有無を検査する encode と streamer が呼ぶ。
func FFmpegDecoders(ctx context.Context, ffmpeg string) ([]byte, error) {
	out, err := CommandOutput(ctx, ffmpeg, "-hide_banner", "-decoders")
	if err != nil {
		return nil, fmt.Errorf("listing decoders of ffmpeg %q: %w", ffmpeg, err)
	}
	return out, nil
}

// ValidateLibARIBCaption は字幕を有効にした構成で、実際に使う ffmpeg が
// libaribcaption デコーダを持つこと（`ffmpeg -decoders` の出力 decoders）を起動時に
// 検査する。Debian の apt 版 ffmpeg には通常含まれないため、
// 設定したのに字幕だけ黙って消える状態を許さない。
func ValidateLibARIBCaption(decoders []byte, scope string) error {
	if !strings.Contains(string(decoders), "libaribcaption") {
		return fmt.Errorf("%s subtitles require an ffmpeg build with libaribcaption decoder", scope)
	}
	return nil
}
