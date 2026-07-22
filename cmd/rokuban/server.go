package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/worker"
)

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the rokuban server",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, err := cmd.Flags().GetBool("all")
			if err != nil {
				return err
			}
			if !all {
				return fmt.Errorf("--all is currently required")
			}

			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			pool, err := db.NewPool(ctx, cfg.DB)
			if err != nil {
				return err
			}
			defer pool.Close()

			workers := worker.NewWorkers()
			client, err := worker.NewClient(pool, workers)
			if err != nil {
				return err
			}

			slog.Info("starting river client")
			if err := client.Start(ctx); err != nil {
				return fmt.Errorf("starting river client: %w", err)
			}

			<-ctx.Done()
			slog.Info("shutting down")

			_ = client.Stop(context.Background())
			return nil
		},
	}

	cmd.Flags().Bool("all", false, "run all roles")

	return cmd
}
