package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/watcher"
	"github.com/fetburner/rokuban/internal/worker"
	"github.com/fetburner/rokuban/web"
)

var (
	allRoles = []string{"api", "worker", "ruler", "reconciler", "watcher", "streamer"}
	// シングルトンロールは pg_advisory_lock でリーダー選出し、1 プロセスだけが実行する
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

			slog.Info("starting server", "roles", roles)

			eg, egCtx := errgroup.WithContext(ctx)

			if slices.Contains(roles, "api") {
				distFS, subErr := fs.Sub(web.DistFS, "dist")
				if subErr != nil {
					return fmt.Errorf("embedded dist/ not found: %w", subErr)
				}
				router := api.NewRouter(cfg.Server.AllowedHosts, distFS, pool)
				srv := &http.Server{Addr: cfg.Server.Listen, Handler: router}

				eg.Go(func() error {
					slog.Info("starting http server", "addr", cfg.Server.Listen)
					if httpErr := srv.ListenAndServe(); httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
						return fmt.Errorf("http server: %w", httpErr)
					}
					return nil
				})
				eg.Go(func() error {
					<-egCtx.Done()
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer shutdownCancel()
					return srv.Shutdown(shutdownCtx)
				})
			}

			// River client（worker と watcher で共有）
			var riverClient *river.Client[pgx5.Tx]
			if slices.Contains(roles, "worker") || slices.Contains(roles, "watcher") {
				mc := mirakc.NewClient(cfg.Mirakc.URL, nil)
				workers := worker.NewWorkers(&worker.IngestDeps{
					MirakcClient: mc,
					Pool:         pool,
					MediaDir:     cfg.Storage.MediaDir,
				})
				var clientErr error
				riverClient, clientErr = worker.NewClient(pool, workers, cfg.Ingest.Concurrency)
				if clientErr != nil {
					return clientErr
				}
			}

			if slices.Contains(roles, "worker") {
				if startErr := riverClient.Start(ctx); startErr != nil {
					return fmt.Errorf("starting river client: %w", startErr)
				}
				defer func() {
					stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer stopCancel()
					_ = riverClient.Stop(stopCtx)
				}()
			}

			// シングルトンロールは監督ループで管理する。
			// ロック取得を定期試行し、取得後は heartbeat で監視する。
			for _, r := range roles {
				if !slices.Contains(singletonRoles, r) {
					continue
				}
				roleName := r
				eg.Go(func() error {
					roleFunc := func(ctx context.Context) error {
						slog.Info("role started", "role", roleName)
						<-ctx.Done()
						slog.Info("role stopped", "role", roleName)
						return ctx.Err()
					}
					switch roleName {
					case "watcher":
						mc := mirakc.NewClient(cfg.Mirakc.URL, nil)
						w := watcher.New(watcher.DefaultSite, mc, pool, riverClient, nil)
						roleFunc = w.Run
					case "reconciler":
						mc := mirakc.NewClient(cfg.Mirakc.URL, nil)
						rec := reconciler.New(watcher.DefaultSite, mc, pool, nil)
						roleFunc = rec.Run
					}
					return role.RunSingleton(egCtx, pool, roleName, roleFunc, nil)
				})
			}

			eg.Go(func() error {
				<-egCtx.Done()
				return nil
			})

			err = eg.Wait()
			slog.Info("shutting down")
			return err
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
