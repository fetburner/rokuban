package main

import (
	"io"
	"log/slog"
	"os"

	"github.com/fetburner/rokuban/internal/config"
)

// configureLogging は log.level / log.format から slog.Handler を構成し、
// パッケージ既定ロガー（slog.Default）に据える。loadConfig（全サブコマンド
// 共通の config 読み込み入口）から呼ぶことで、server はもちろん migrate /
// rescue / enqueue / catalog / shadow-diff にも同じ設定が効く。
func configureLogging(cfg config.LogConfig) {
	slog.SetDefault(slog.New(newLogHandler(cfg, os.Stderr)))
}

// newLogHandler は configureLogging の中身を出力先を差し替え可能な形で
// 切り出したもの（slog.SetDefault を触らずにテストするため）。
//
// **空文字は defaults() と同じ扱いにする。** LogConfig.validate は空文字を
// 「未設定」として通す（`level: ${VAR}` の展開で空文字が入る構成があるため。
// internal/config の TestLoad_EmptyLogValuesAreTreatedAsUnset）。ここで
// フォールバックしないと、その構成だけ既定と異なるロガーになる。
func newLogHandler(cfg config.LogConfig, w io.Writer) slog.Handler {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default: // "info" と "" はどちらも既定の Info 相当
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts) // "json" と "" はどちらも既定の json
}
