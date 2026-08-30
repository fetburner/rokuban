package main

import (
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
)

// loadConfig は --config で指定された設定ファイルを読み、成功したらそこから
// ロガーを構成する（configureLogging）。全サブコマンド（server / migrate /
// rescue / enqueue / catalog / shadow-diff / config validate）がここを通る
// ので、ロガーの設定場所もここに 1 箇所だけ置く。
//
// **Load 自体が失敗したときのログは既定のまま出る。** 設定を読めていないので
// 適用しようがない。呼び出し元は返ったエラーを自分でログ / 標準エラーに出す。
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	configureLogging(cfg.Log)
	return cfg, nil
}

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
// internal/config の TestLoad_EmptyLogValuesAreTreatedAsUnset）。
//
// **`config.defaults()` の info/json と意図的に二重に持っている。** 空文字の
// 構成は defaults() の代入（YAML にキーが無いときだけ効く）を経由せずここへ
// 届くので、defaults() を読むだけでは揃わない。値がずれていないかは
// TestNewLogHandler_EmptyIsDefault がリテラルで固定する。
func newLogHandler(cfg config.LogConfig, w io.Writer) slog.Handler {
	// slog.Level の zero value は LevelInfo なので、UnmarshalText が空文字や
	// 未知の値で失敗しても level は Info のまま残る。level はロード時に
	// LogConfig.validate 済み（logLevels の 4 値か空文字のみ）なので、
	// ここでのエラーは実質「空文字 = 未設定」の一択。
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Level))

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts) // "json" と "" はどちらも既定の json
}
