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
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/notifier"
	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/streamer"
	"github.com/fetburner/rokuban/internal/watcher"
	"github.com/fetburner/rokuban/internal/worker"
	"github.com/fetburner/rokuban/web"
)

var (
	allRoles = []string{"api", "worker", "watcher", "streamer", "notifier"}
	// シングルトンロールは pg_advisory_lock でリーダー選出し、1 プロセスだけが実行する。
	// ruler / reconciler はここに含まれない — シングルトンではなく worker が引く River
	// ジョブ（ruler_pass / reconcile_pass）になったため（docs/data.md §2、
	// docs/overview.md「ロールは『プロセスの形』を表し、『どの仕事をするか』は
	// 表さない」、issue #24 M2-17）。notifier も同じ理由でここに含まれない —
	// 複数レプリカが各自 LISTEN して自分の SSE クライアントに配るだけなので、
	// 1 プロセスに絞る必要がない（docs/data.md §3、issue #24 M2-19）。
	// シングルトンは watcher だけになった。
	singletonRoles = []string{"watcher"}
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

			// HTTP リスナーはロールに関わらず 1 本立てる。ヘルスチェックと
			// /metrics はどのロールでも scrape できる必要があるため
			// （worker だけの Pod でも滞留メトリクスを取りたい）。
			// SPA・SSE・バイト配信は担当ロールのときだけ登録する。
			{
				routerCfg := api.RouterConfig{
					AllowedHosts:    cfg.Server.AllowedHosts,
					Pool:            pool,
					MetricsRegistry: metrics.NewRegistry(metrics.NewBacklogCollector(pool, watcher.DefaultSite)),
				}

				if slices.Contains(roles, "api") {
					distFS, subErr := fs.Sub(web.DistFS, "dist")
					if subErr != nil {
						return fmt.Errorf("embedded dist/ not found: %w", subErr)
					}
					routerCfg.DistFS = distFS

					// ルール作成/更新/削除のヒントで ruler_pass を投入するための
					// insert-only クライアント。api は mirakc に問い合わせず ffmpeg も
					// 実行しない（不変条件）ため、worker.NewWorkers のフルのワーカー群は
					// 登録しない（InsertTx だけできれば足りる。docs/recording.md §3.1）。
					apiRiverClient, apiRiverErr := worker.NewInsertOnlyClient(pool)
					if apiRiverErr != nil {
						return apiRiverErr
					}
					routerCfg.RiverClient = apiRiverClient
					// ルール保存時の encodeProfiles 存在検証用（名前集合だけ。
					// ffmpeg の LookPath はしない。issue #64）。
					routerCfg.EncodeProfileNames = cfg.Encode.ProfileNames()
				}

				// バイト配信は streamer、SSE のヒント配送は notifier の担当
				// （不変条件 1、issue #24 M2-19）。api はどちらにも依存しない
				// 純粋なリクエスト/レスポンス層になる。両方を同一プロセスに
				// 同居させる場合（monolith / --all）は Mounters で束ねる。
				var mounters api.Mounters
				if slices.Contains(roles, "streamer") {
					mounters = append(mounters, streamer.New(pool, streamer.Config{
						MediaDir:      cfg.Storage.MediaDir,
						AccelLocation: cfg.Storage.AccelLocation,
					}))
				}
				if slices.Contains(roles, "notifier") {
					// notifier はシングルトンではない。各レプリカが自分で LISTEN して
					// 自分にぶら下がる SSE クライアントに配るだけなので、レプリカを
					// 増やしても Redis アダプタ等の追加基盤は要らない（docs/data.md §3）。
					hub := notifier.NewEventHub()
					mounters = append(mounters, hub)
					eg.Go(func() error {
						if hubErr := hub.Run(egCtx, pool); hubErr != nil && !errors.Is(hubErr, context.Canceled) {
							return fmt.Errorf("notifier: %w", hubErr)
						}
						return nil
					})
				}
				if len(mounters) > 0 {
					routerCfg.Mounter = mounters
				}

				srv := &http.Server{Addr: cfg.Server.Listen, Handler: api.NewRouter(routerCfg)}

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
				// ffmpeg/ffprobe の存在検査は worker ロール起動時だけ
				// （不変条件 4。api-only では呼ばない。issue #64）。
				if slices.Contains(roles, "worker") {
					if toolErr := cfg.Encode.ValidateTools(); toolErr != nil {
						return toolErr
					}
				}
				mc := mirakc.NewClient(cfg.Mirakc.URL, nil)
				workers := worker.NewWorkers(&worker.Deps{
					MirakcClient:             mc,
					Pool:                     pool,
					MediaDir:                 cfg.Storage.MediaDir,
					EpgRetentionGrace:        cfg.Epg.RetentionGrace,
					RulerRetentionGrace:      cfg.Epg.RetentionGrace,
					RulerMaxDeletesPerPass:   cfg.Ruler.MaxDeletesPerPass,
					ReconcileStartDelayGrace: cfg.Reconciler.StartDelayGrace,
					IngestStallTimeout:       cfg.Ingest.StallTimeout,
				})
				clientCfg := worker.ClientConfig{
					IngestConcurrency:    cfg.Ingest.Concurrency,
					EncodeConcurrency:    cfg.Encode.Concurrency,
					ThumbnailConcurrency: cfg.Encode.ThumbnailConcurrency,
					EpgSyncInterval:      cfg.Epg.SyncInterval,
					PeriodicJobs:         cfg.Worker.PeriodicJobs,
					Queues:               cfg.Worker.Queues,
				}
				// 定期ジョブ（epg_sync / tuner_sync / ruler_pass / reconcile_pass /
				// record_sweep）は worker 側が投入する（mirakc に触るのも各ジョブの
				// ヒント経路をまとめるのも worker）。
				if slices.Contains(roles, "worker") {
					clientCfg.EpgSyncSite = watcher.DefaultSite
					clientCfg.TunerSyncSite = watcher.DefaultSite
					clientCfg.RulerPassSite = watcher.DefaultSite
					clientCfg.ReconcilePassSite = watcher.DefaultSite
					clientCfg.RecordSweepSite = watcher.DefaultSite
				}
				var clientErr error
				riverClient, clientErr = worker.NewClient(pool, workers, clientCfg)
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
					// shutdown 中は呼び出し元に返す先がない。30 秒のタイムアウトを
					// 付けている以上「実行中のジョブが終わらずタイムアウトした」は
					// 起こりうるので、握り潰さずログに残す（issue #58）。
					if err := riverClient.Stop(stopCtx); err != nil {
						slog.Error("stopping river client", "err", err)
					}
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
						w := watcher.New(watcher.DefaultSite, mc, pool, riverClient, worker.NewIngestArgs)
						roleFunc = w.Run
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
	cmd.Flags().StringSlice("roles", nil, "roles to run (comma-separated: api,worker,watcher,streamer,notifier)")
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
