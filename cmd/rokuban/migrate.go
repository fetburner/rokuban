package main

import (
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	rivermigrate "github.com/fetburner/rokuban/internal/jobq/riveradapter/migrate"
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
	var mediaCopyWritersStopped bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run all pending migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			dsn := cfg.DB.DSN()
			if err := db.CheckMediaCopyMigrationSafety(cmd.Context(), dsn, mediaCopyWritersStopped); err != nil {
				return err
			}
			if err := db.MigrateUp(cmd.Context(), dsn); err != nil {
				return err
			}
			return rivermigrate.Up(cmd.Context(), dsn)
		},
	}
	cmd.Flags().BoolVar(&mediaCopyWritersStopped, "media-copy-writers-stopped", false,
		"confirm that old media copy writers are stopped before a non-rolling media copy migration")
	return cmd
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
