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
	"github.com/fetburner/rokuban/internal/webhook"
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

			pool, err := db.NewPool(ctx, cfg.DB, roles)
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
					Site:            cfg.Mirakc.Site,
					MetricsRegistry: metrics.NewRegistry(metrics.NewBacklogCollector(pool, cfg.Mirakc.Site)),
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

					// ライブ視聴（issue #91）。live.enabled が true のときだけ ffmpeg の
					// LookPath 検査を行う --- 公式イメージ（ffmpeg 無し）で streamer を
					// 起動する構成（録画配信 / サムネイルのみ）を壊さない
					// （不変条件 4、cfg.Encode.ValidateTools の条件付き検査と同じ形）。
					if cfg.Live.Enabled {
						if toolErr := cfg.Live.ValidateTools(); toolErr != nil {
							return toolErr
						}

						liveProfiles := make([]streamer.LiveProfile, 0, len(cfg.Live.Profiles))
						for _, p := range cfg.Live.Profiles {
							liveProfiles = append(liveProfiles, streamer.LiveProfile{
								Name:           p.Name,
								VideoCodec:     p.VideoCodec,
								AudioCodec:     p.AudioCodec,
								Height:         p.Height,
								Preset:         p.Preset,
								SegmentSeconds: p.SegmentSeconds,
								PlaylistSize:   p.PlaylistSize,
								ExtraArgs:      p.ExtraArgs,
							})
						}
						liveMirakcClient := mirakc.NewClient(cfg.Mirakc.URL, nil)
						liveStreamer := streamer.NewLive(liveMirakcClient, cfg.Mirakc.Site, streamer.LiveConfig{
							Enabled:       true,
							FFmpeg:        cfg.Live.FFmpeg,
							SegmentDir:    cfg.Live.SegmentDir,
							MaxSessions:   cfg.Live.MaxSessions,
							IdleTimeout:   cfg.Live.IdleTimeout,
							TunerPriority: cfg.Live.TunerPriority,
							Profiles:      liveProfiles,
						})
						mounters = append(mounters, liveStreamer)
						// idle GC ループ。ctx（egCtx）が終わったら全セッションを止めて
						// mirakc の接続を閉じる（チューナー解放。crash-only の唯一の
						// 例外の後始末。docs/overview.md §crash-only）。
						eg.Go(func() error { return liveStreamer.Run(egCtx) })
					}
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

			// 汎用 webhook（M3-11）。URL 空なら no-op。worker（record_sweep）と
			// watcher の両方から同じ Client を使う。
			webhookClient := webhook.New(cfg.Webhook)

			// River client（worker と watcher で共有）。
			//
			// ロールがそのプロセスの実行する仕事を決める（issue #113）。
			// worker ロールが無いプロセスに worker.NewWorkers のフルのワーカー群
			// （EncodeWorker/ThumbnailWorker を含む）を登録すると、ffmpeg/ffprobe を
			// 検査しないまま encode/thumbnail ジョブを実行しうる（不変条件 4 違反）。
			// resolveRiverClientKind がロールから組み立て方を一意に決め、
			// watcher 単独では api ロールと同じ NewInsertOnlyClient（ingest ジョブの
			// InsertTx 専用、Workers 登録なし・Start 不可）を使う。
			var riverClient *river.Client[pgx5.Tx]
			switch resolveRiverClientKind(roles) {
			case riverClientFull:
				// ffmpeg/ffprobe の存在検査は、実際に encode/thumbnail キューを
				// 購読するときだけ行う（worker.queues で絞った ingest 専用 Pod 等に
				// まで ffmpeg を要求しないため。issue #113 決定 C）。
				if worker.RequiresEncodeTools(cfg.Worker.Queues) {
					if toolErr := cfg.Encode.ValidateTools(); toolErr != nil {
						return toolErr
					}
				}
				mc := mirakc.NewClient(cfg.Mirakc.URL, nil)
				workers := worker.NewWorkers(&worker.Deps{
					MirakcClient:             mc,
					Pool:                     pool,
					MediaDir:                 cfg.Storage.MediaDir,
					Site:                     cfg.Mirakc.Site,
					ScratchDir:               cfg.Storage.ScratchDir,
					Encode:                   cfg.Encode,
					EpgRetentionGrace:        cfg.Epg.RetentionGrace,
					RulerRetentionGrace:      cfg.Epg.RetentionGrace,
					RulerMaxDeletesPerPass:   cfg.Ruler.MaxDeletesPerPass,
					ReconcileStartDelayGrace: cfg.Reconciler.StartDelayGrace,
					IngestStallTimeout:       cfg.Ingest.StallTimeout,
					Webhook:                  webhookClient,
					Cleanup:                  cfg.Cleanup,
				})
				clientCfg := worker.ClientConfig{
					IngestConcurrency:    cfg.Ingest.Concurrency,
					EncodeConcurrency:    cfg.Encode.Concurrency,
					ThumbnailConcurrency: cfg.Encode.ThumbnailConcurrency,
					EpgSyncInterval:      cfg.Epg.SyncInterval,
					PeriodicJobs:         cfg.Worker.PeriodicJobs,
					Queues:               cfg.Worker.Queues,
					// 定期ジョブ（epg_sync / tuner_sync / ruler_pass / reconcile_pass /
					// record_sweep / catalog_export / delete_reconcile）は worker 側が
					// 投入する（mirakc に触るのも各ジョブのヒント経路をまとめるのも
					// worker。riverClientFull は worker ロールがあるときにしか
					// 選ばれないので、ここは無条件に設定してよい）。
					EpgSyncSite:       cfg.Mirakc.Site,
					TunerSyncSite:     cfg.Mirakc.Site,
					RulerPassSite:     cfg.Mirakc.Site,
					ReconcilePassSite: cfg.Mirakc.Site,
					RecordSweepSite:   cfg.Mirakc.Site,
					CatalogExport:     true,
					DeleteReconcile:   true,
				}
				var clientErr error
				riverClient, clientErr = worker.NewClient(pool, workers, clientCfg)
				if clientErr != nil {
					return clientErr
				}
			case riverClientInsertOnly:
				var clientErr error
				riverClient, clientErr = worker.NewInsertOnlyClient(pool)
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
						w := watcher.New(cfg.Mirakc.Site, mc, pool, riverClient, worker.NewIngestArgs, webhookClient)
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

// riverClientKind は roles から River クライアントの組み立て方を分類する。
type riverClientKind int

const (
	// riverClientNone は worker も watcher も無いプロセス。River クライアントは
	// 作らない（例: api 単独。api は専用の insert-only クライアントを別途持つ）。
	riverClientNone riverClientKind = iota
	// riverClientFull は worker ロールがあるプロセス。worker.NewWorkers の
	// フルのワーカー群（EncodeWorker/ThumbnailWorker を含む）を登録し、
	// Start して実際にジョブを実行する。
	riverClientFull
	// riverClientInsertOnly は watcher ロールだけがあり worker ロールが無い
	// プロセス。ingest ジョブの投入（InsertTx）にしか使わないので、
	// worker.NewInsertOnlyClient と同じ最小構成にする --- ffmpeg/ffprobe に
	// 依存するワーカーを登録・実行しない（不変条件 4、issue #113）。
	riverClientInsertOnly
)

// resolveRiverClientKind はロール集合から riverClientKind を一意に決める。
//
// worker ロールが最優先される: worker と watcher を同一プロセスで動かす
// 構成（monolith / --all）では、watcher の ingest ジョブ投入と worker の
// ジョブ実行を同じ River クライアントで共有する（既存の挙動を変えない）。
// worker ロールが無く watcher だけの場合にのみ riverClientInsertOnly を返し、
// フルのワーカー群が登録されるのを防ぐ。
func resolveRiverClientKind(roles []string) riverClientKind {
	switch {
	case slices.Contains(roles, "worker"):
		return riverClientFull
	case slices.Contains(roles, "watcher"):
		return riverClientInsertOnly
	default:
		return riverClientNone
	}
}
