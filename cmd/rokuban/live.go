package main

import (
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/streamer"
)

// convertLiveConfig は config.LiveConfig を streamer.LiveConfig に変換する。
//
// **streamer は internal/config を import しない**（streamer.LiveConfig が
// config.LiveConfig と別構造体である理由。docs/configuration.md「live.profiles
// は encode.profiles とは別の構造体」と同じ形）ので、フィールドごとの変換が
// ここに要る。フィールドを 1 本足し忘れても黙って消えるだけ（コンパイルエラーに
// ならない）なので、TestConvertLiveConfig_NoFieldLeftBehind が reflect で
// 「変換後にゼロ値のフィールドが残っていないか」を機械的に固定する
// （issue #321 決定コメント §6）。
func convertLiveConfig(c config.LiveConfig) streamer.LiveConfig {
	profiles := make([]streamer.LiveProfile, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		profiles = append(profiles, streamer.LiveProfile{
			Name:           p.Name,
			VideoCodec:     p.VideoCodec,
			AudioCodec:     p.AudioCodec,
			Height:         p.Height,
			Scaler:         p.Scaler,
			CRF:            p.CRF,
			QP:             p.QP,
			Preset:         p.Preset,
			SegmentSeconds: p.SegmentSeconds,
			PlaylistSize:   p.PlaylistSize,
			ExtraArgs:      p.ExtraArgs,
		})
	}
	return streamer.LiveConfig{
		Enabled:        true,
		FFmpeg:         c.FFmpeg,
		FFprobe:        c.FFprobe,
		SegmentDir:     c.SegmentDir,
		MaxSessions:    c.MaxSessions,
		IdleTimeout:    c.IdleTimeout,
		TunerPriority:  c.TunerPriority,
		Captions:       c.Captions,
		HWAccel:        c.HWAccel,
		InputExtraArgs: c.InputExtraArgs,
		Profiles:       profiles,
	}
}
