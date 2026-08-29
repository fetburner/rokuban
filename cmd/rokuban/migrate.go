package main

import (
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
)

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Database migration utilities",
	}

	cmd.AddCommand(newMigrateUpCmd())
	cmd.AddCommand(newMigrateDownCmd())

	return cmd
}

func newMigrateUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Run all pending migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return db.MigrateUp(cmd.Context(), cfg.DB.DSN())
		},
	}
}

func newMigrateDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Roll back the last migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return db.MigrateDown(cmd.Context(), cfg.DB.DSN())
		},
	}
}

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
