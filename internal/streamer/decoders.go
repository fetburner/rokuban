// measure
package streamer

import (
	"context"
	"fmt"
	"regexp"

	"github.com/fetburner/rokuban/internal/mediaexec"
)

// ValidateFFmpegDecoders は streamer ロールの起動時に ffmpeg のデコーダを検査する。
// ライブ視聴と録画 HLS は MPEG-2 TS の原本を読むので mpeg2video を常に要る。
// captions が true なら、字幕のために libaribcaption も要る。
// `ffmpeg -decoders` は 1 回だけ実行し、その出力で両方を判定する。
// cmd/rokuban が cfg.Live.ValidateTools の後で呼ぶ（不変条件 4）。
func ValidateFFmpegDecoders(ctx context.Context, ffmpeg string, captions bool) error {
	decoders, err := mediaexec.FFmpegDecoders(ctx, ffmpeg)
	if err != nil {
		return err
	}
	if err := validateFFmpegDecoder(decoders, "mpeg2video", "live source MPEG-2 TS"); err != nil {
		return err
	}
	if captions {
		if err := mediaexec.ValidateLibARIBCaption(decoders, "live"); err != nil {
			return err
		}
	}
	return nil
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
