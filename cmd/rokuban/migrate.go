package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	rivermigrate "github.com/fetburner/rokuban/internal/jobq/riveradapter/migrate"
	"github.com/fetburner/rokuban/internal/mysqldb"
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
			if cfg.DB.Dialect == config.DBDialectMySQL {
				return migrateMySQLUp(cmd.Context(), cfg)
			}
			dsn := cfg.DB.DSN()
			if err := db.MigrateUp(cmd.Context(), dsn); err != nil {
				return err
			}
			if cfg.Worker.Engine == config.JobEngineRiver {
				return rivermigrate.Up(cmd.Context(), dsn)
			}
			return nil
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
			if cfg.DB.Dialect == config.DBDialectMySQL {
				return migrateMySQLDown(cmd.Context(), cfg)
			}
			return db.MigrateDown(cmd.Context(), cfg.DB.DSN())
		},
	}
}

// migrateMySQLUp は MySQL の埋め込み migration を適用する。River は MySQL 構成では使わない（config の検査）。
func migrateMySQLUp(ctx context.Context, cfg *config.Config) error {
	h, err := openDB(ctx, cfg, nil, 0, 0)
	if err != nil {
		return err
	}
	defer h.Close()
	return mysqldb.MigrateUp(ctx, h.my)
}

// migrateMySQLDown は MySQL の migration を 1 ステップ戻す。
func migrateMySQLDown(ctx context.Context, cfg *config.Config) error {
	h, err := openDB(ctx, cfg, nil, 0, 0)
	if err != nil {
		return err
	}
	defer h.Close()
	return mysqldb.MigrateDown(ctx, h.my)
}
