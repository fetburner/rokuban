package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/worker"
)

var (
	allRoles       = []string{"api", "worker", "ruler", "reconciler", "watcher", "streamer"}
	singletonRoles = []string{"ruler", "reconciler", "watcher"}
)

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the rokuban server",
		RunE: func(cmd *cobra.Command, args []string) error {
			roles, err := resolveRoles(cmd)
			if err != nil {
				return err
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

			var releases []func()
			defer func() {
				for _, r := range releases {
					r()
				}
			}()

			activeRoles := make([]string, 0, len(roles))
			for _, r := range roles {
				if slices.Contains(singletonRoles, r) {
					acquired, release, lockErr := role.TryAcquire(ctx, pool, r)
					if lockErr != nil {
						return lockErr
					}
					if !acquired {
						continue
					}
					releases = append(releases, release)
				}
				activeRoles = append(activeRoles, r)
			}

			slog.Info("starting server", "roles", activeRoles)

			if slices.Contains(activeRoles, "worker") {
				workers := worker.NewWorkers()
				client, clientErr := worker.NewClient(pool, workers)
				if clientErr != nil {
					return clientErr
				}

				if startErr := client.Start(ctx); startErr != nil {
					return fmt.Errorf("starting river client: %w", startErr)
				}
				defer func() {
					_ = client.Stop(context.Background())
				}()
			}

			<-ctx.Done()
			slog.Info("shutting down")
			return nil
		},
	}

	cmd.Flags().Bool("all", false, "run all roles")
	cmd.Flags().StringSlice("roles", nil, "roles to run (comma-separated: api,worker,ruler,reconciler,watcher,streamer)")
	cmd.MarkFlagsMutuallyExclusive("all", "roles")
	cmd.MarkFlagsOneRequired("all", "roles")

	return cmd
}

func resolveRoles(cmd *cobra.Command) ([]string, error) {
	all, err := cmd.Flags().GetBool("all")
	if err != nil {
		return nil, err
	}
	if all {
		return allRoles, nil
	}

	roles, err := cmd.Flags().GetStringSlice("roles")
	if err != nil {
		return nil, err
	}
	for _, r := range roles {
		if !slices.Contains(allRoles, r) {
			return nil, fmt.Errorf("unknown role: %q (valid: %s)", r, strings.Join(allRoles, ", "))
		}
	}
	return roles, nil
}
