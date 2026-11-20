package encode

import (
	"context"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/mediaexec"
)

// ValidateDecoders は、subtitles: webvtt のプロファイルが 1 つでもあるとき、
// encode.ffmpeg が libaribcaption デコーダを持つことを起動時に検査する。
// デコーダの一覧は ffmpeg -decoders を 1 回だけ実行して得る。字幕を使わない構成では
// ffmpeg を実行しない。worker ロールの起動時に cmd/rokuban が
// cfg.Encode.ValidateTools の後で呼ぶ（不変条件 4）。
func ValidateDecoders(ctx context.Context, cfg config.EncodeConfig) error {
	for _, p := range cfg.Profiles {
		if p.Subtitles != "webvtt" {
			continue
		}
		decoders, err := mediaexec.FFmpegDecoders(ctx, cfg.FFmpeg)
		if err != nil {
			return err
		}
		return mediaexec.ValidateLibARIBCaption(decoders, "encode")
	}
	return nil
}
