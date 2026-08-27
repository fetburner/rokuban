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
	"github.com/fetburner/rokuban/internal/config"
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

const (
	// httpReadHeaderTimeout はリクエストヘッダーを読み切るまでの上限。Rokuban は
	// nginx 等のリバースプロキシを必須にせず `--all` で単体動作することを要件に
	// しており（docs/api/deployment.md §単一バイナリの自己完結）、前段プロキシの
	// タイムアウトを前提にできない構成で直接 listen しうる。この下限はアプリ
	// 自身が持つ必要がある。無ければヘッダー送信を極端に遅くするクライアントが
	// 接続と goroutine を無期限に握り、多重接続でファイルディスクリプタ／メモリを
	// 枯渇させられる（issue #368）。
	//
	// 10 秒という値は、slow-header 接続を有限時間で切る基準として mirakc
	// クライアント側の responseHeaderTimeout（internal/mirakc/client.go の
	// 30 秒。応答待ちなので許容が広い）より短く取った、という相対関係だけを
	// 根拠にしている。通常のクライアントのヘッダー送信にどれだけ掛かるかは
	// 未計測。ReadHeaderTimeout が実際に slow-header 接続を切ることは
	// TestNewHTTPServer_SlowHeaderConnectionIsClosed で、この値が本番の配線に
	// 載っていることは TestNewProductionHTTPServer_UsesReviewedTimeouts と
	// TestServerSlowHeaderConnectionIsClosed で確認。
	httpReadHeaderTimeout = 10 * time.Second

	// httpIdleTimeout は keep-alive 接続がリクエストとリクエストの間で無期限に
	// 張られたままにならないための上限。net/http の IdleTimeout は「次の
	// リクエストの到着待ち」の間だけ働き、ハンドラが応答を書き続けている間
	// （SSE の hub.Run や HLS のセグメント配信）は対象にならないため、WriteTimeout
	// を一律に設定する場合と異なり長寿命配信を切らない —— この主張は
	// TestNewHTTPServer_LongRunningHandlerNotCutByIdleTimeout で、ハンドラが
	// IdleTimeout を超えて書き続ける間も接続が切られないことを実測して確認して
	// ある。アイドルな keep-alive 接続が期限で切られる側は
	// TestNewHTTPServer_IdleConnectionIsClosedByIdleTimeout で確認。ReadTimeout
	// を設定していないため明示しないと無期限になる。
	httpIdleTimeout = 120 * time.Second

	// httpShutdownTimeout は SIGTERM 後に進行中のリクエストを書き終えるのを待つ
	// 上限（`http.Server.Shutdown`）。
	//
	// **プロセスの停止予算では River の drain と足し合わせる**（重なって進むが、
	// 上界は和になる。shutdownBudget のコメント参照）。`deploy/k8s/manifests_test.go` の
	// TestTerminationBudgetCoversPreStop は同じ 10 という数字をリテラルで持って
	// いる（実装の定数は参照していない --- 参照すると両方を同時に変えたときに
	// 何も主張しなくなるため）。**したがってここを変えてもあのテストは緑のまま**
	// なので、変えるときは向こうのリテラルと api.yaml の猶予も揃える。
	httpShutdownTimeout = 10 * time.Second

	// hardStopGrace は soft stop の猶予が切れて River が work ctx を cancel した
	// あと、**畳み終えるのを待つ**時間。
	//
	// ctx を切られた実行中のジョブが Work から戻り、River の completer が
	// その結果（`available` への差し戻しとエラー行）を書き終えるまでに要する。
	// **所要は測っていない。** プロセス自身が持つもう 1 つの停止予算
	// （httpShutdownTimeout）と同じ大きさに揃えた、という相対関係だけを根拠に
	// している。
	hardStopGrace = 10 * time.Second
)

// shutdownBudget は `riverClient.Stop` を待つ上限を返す。**soft stop の猶予より
// 必ず長い。**
//
// 短いと、猶予の内側で完走するはずのジョブを**プロセスが先に抜けることで**
// 打ち切る。しかもその打ち切りは ctx の cancel ですらない（プロセスが終わる
// だけ）ので、行は `running` のまま残り、回収は `JobRescuer`（リーダーだけが
// 動かす保守サービス）に委ねることになる --- ロール分割構成では常駐する River
// クライアントが 1 つも無いので、誰も回収しない（docs/operations.md §5 の
// スケーラのクエリの節と同じ族の問題）。一方、プロセスが自分でエスカレートして
// 畳めば行は `available` に戻り、次に起きた worker が引き直せる。**「試行を
// 1 つ潰す」と「誰も引き直せない」の差**である。
//
// **時計が 2 つあるので余裕が要る。** River の escalate は SIGTERM の瞬間から
// 測る（`fetchCtx` が start ctx から派生し、その Done で soft stop タイマーが
// 走り出す）が、この締切は `eg.Wait()` が戻ってから測る。同じ区間を別の原点で
// 測っているので、`soft` ちょうどでは足りない。
//
// **これはプロセス全体の停止予算ではない。** 2 つの停止（HTTP と River の drain）
// は SIGTERM を契機に**重なって**進むので実際の停止はもっと早いことが多いが、
// この締切の原点が `eg.Wait()` の後にある以上、**上界は和**になる。k8s の
// `terminationGracePeriodSeconds` が包むべきはその和のほうである
// （docs/operations.md §5「Deployment 併用時」の足し算）。
func shutdownBudget(softStopTimeout time.Duration) time.Duration {
	return softStopTimeout + hardStopGrace
}

// stopRiverForShutdown は常駐 worker の停止で `riverClient.Stop` に与える締切を
// 決めて呼ぶ。
//
// **締切を固定値にしない**ことがこの関数の全部である。固定だと、
// `--soft-stop-timeout` を長く取った構成（数時間の encode を載せる ScaledJob /
// Deployment）で「River はまだ猶予の内側なのにプロセスだけ先に抜ける」形になり、
// drain が黙って固定値で打ち切られる（shutdownBudget のコメント）。
//
// **RunE の defer から切り出してあるのはテストのため。** 実 DB で測ろうとすると
// 「猶予より長く走るジョブ」が要り、テストの所要が猶予そのものになる
// （TestStopRiverForShutdown_DeadlineFollowsSoftStopTimeout と
// TestServerCmd_SigtermDrainsRunningJob のコメント）。
func stopRiverForShutdown(stopRiver func(context.Context) error, softStopTimeout time.Duration) {
	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget(softStopTimeout))
	defer cancel()
	// shutdown 中は呼び出し元に返す先がない。タイムアウトを付けている以上
	// 「実行中のジョブが終わらずタイムアウトした」は起こりうるので、
	// 握り潰さずログに残す（issue #58）。
	if err := stopRiver(stopCtx); err != nil {
		slog.Error("stopping river client", "err", err)
	}
}

// newProductionHTTPServer は本番の HTTP サーバーを構築する。
//
// タイムアウトを引数で受けず定数を自分で選ぶのが要点 —— newHTTPServer は同じ型
// （time.Duration）の引数を 2 つ隣接した位置で受けるため、呼び出し側で入れ替えても
// コンパイルは通り、10 秒と 120 秒が静かに逆になる（slow-header の窓が 12 倍に
// 開く）。呼び出し側に Duration を渡させないことで、その取り違えを表現不可能に
// する（不変条件 10「表現不可能にする方が強い」と同じ考え）。
func newProductionHTTPServer(addr string, handler http.Handler) *http.Server {
	return newHTTPServer(addr, handler, httpReadHeaderTimeout, httpIdleTimeout)
}

// newHTTPServer は Rokuban の HTTP サーバーを共通のタイムアウト設定で構築する。
// **本番の呼び出し側はこれを直接使わず newProductionHTTPServer を使う** ——
// readHeaderTimeout / idleTimeout を引数で受けているのは、テストが同じ配線を
// 数百 ms オーダーの値で通してタイムアウト動作を実測できるようにするため
// （実装の定数をテストが直接参照すると、定数を変えても落ちないテストになる）。
//
// WriteTimeout は意図的に設定しない —— SSE（notifier）・HLS（streamer）は
// レスポンスを長時間書き続けるため、一律の WriteTimeout はこれらを途中で
// 切断してしまう（エンドポイント特性ごとの判断が要る。issue #368「罠」）。
//
// MaxHeaderBytes も設定しない（net/http の既定である DefaultMaxHeaderBytes =
// 1MiB を使う）。1MiB に届くヘッダーを送るクライアントは想定しておらず（実測は
// していない）、slow-header 接続そのものは httpReadHeaderTimeout が有限時間で
// 切るので、ヘッダーサイズ側に別の上限を追加する理由が無いと判断した（issue
// #368「含むもの」2）。
func newHTTPServer(addr string, handler http.Handler, readHeaderTimeout, idleTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// warnIfAllowedHostsEmpty は server.allowed_hosts が空のまま起動したときに
// WARN ログを出す。
//
// 空は Host ヘッダー検証（DNS rebinding 対策。アプリ内に認証を持たないため、
// これがある構成での唯一の防壁 —— internal/api.AllowedHosts のコメント参照）を
// 丸ごとスキップする「意図した緩和」（LAN 内から IP で直叩きする開発／小規模
// 構成向け）だが、docker-compose の既定構成（.env に `ROKUBAN_ALLOWED_HOSTS` を
// 設定しない）でも同じ形になる。この場合利用者が意識せず、非信頼 LAN や
// ポートフォワード環境で無認証・DNS rebinding 保護なしのサーバーを公開ポートに
// 全開にしてしまう（issue #374）。空でなければ何もしない。
//
// 呼び出しは resolveRoles の後・ロール分岐より前に置く。OpenAPI から生成される
// ルートはロールに関わらず生えるため（このファイル内 LiveEnabled 付近のコメント
// 参照）、Host allowlist はどのロール構成でも防壁として効いている。そのため
// --roles worker や --roles notifier だけのプロセスでも同じ条件で WARN が出る
// のは意図どおり（compose 既定構成に限らず、ロール分割デプロイの全 Pod に対する
// チェックとして機能する）。
//
// 判定（`len(allowedHosts) > 0`）は internal/api.AllowedHosts が検証を丸ごと
// スキップする条件（`internal/api/middleware.go` の `len(normalizedAllowedHosts)
// == 0`）と同じでなければ意味が無い。middleware 側が空要素を除去するなどの変更を
// すると、警告だけが黙って乖離する（「防壁は無いが警告も出ない」構成が作れる）。
// middleware のスキップ条件を触るときはここも合わせる。
func warnIfAllowedHostsEmpty(logger *slog.Logger, allowedHosts []string) {
	if len(allowedHosts) > 0 {
		return
	}
	logger.Warn("server.allowed_hosts is empty: Host header validation (DNS rebinding protection) is disabled; set server.allowed_hosts before exposing this port beyond localhost")
}

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
			warnIfAllowedHostsEmpty(slog.Default(), cfg.Server.AllowedHosts)

			// このプロセスが引くキューを --queues と worker.queues から決める。
			// 以降の判定（validateSiteBinding / RequiresEncodeTools /
			// ClientConfig.Queues）はすべてこの解決済みの集合を見る ---
			// 片方だけが config を直接見ると、argv で絞った Pod が
			// 「絞ったつもりで検査だけ全キュー基準」になる（resolveWorkerQueues）。
			queues, err := resolveWorkerQueues(cmd, cfg.Worker.Queues, roles)
			if err != nil {
				return err
			}
			// 1 件消化モード（KEDA ScaledJob の Job が自分で終了するための起動形態。
			// worker.OnceGate、docs/operations.md §5）。
			onceGate, onceIdleTimeout, err := resolveOnce(cmd, roles)
			if err != nil {
				return err
			}
			// SIGTERM で実行中のジョブを打ち切らない（drain する）ための猶予。
			// 1 件消化モードかどうかに関わらず効く --- `--once` の Job も
			// ノード退避やローリング更新で SIGTERM を受ける（そのときの
			// 停止経路は常駐 worker と同じ）。
			softStopTimeout, err := resolveSoftStopTimeout(cmd, roles)
			if err != nil {
				return err
			}

			// このプロセスが束縛される mirakc サイトを --sites から決める
			// （config キーにしない。issue #183 M4-11「含むもの」4）。
			bound, err := resolveSiteBinding(cmd, cfg.Registry())
			if err != nil {
				return err
			}
			if err := validateSiteBinding(roles, bound, queues); err != nil {
				return err
			}
			// site 名を site 単位のキューに修飾したときに River の 64 文字上限を
			// 超えないことを検証する。internal/config はレジストリの site 名を
			// ロード時に既にこの上限の範囲（config.MirakcSiteNameMaxLen）で検査
			// しているので、ここは site 名が config 以外の経路から来る場合に
			// 備えた最後の砦（worker.ValidateSiteForQueueNames の doc コメント参照）。
			for _, s := range bound {
				if err := worker.ValidateSiteForQueueNames(s.Site); err != nil {
					return err
				}
			}
			// boundSite は「ちょうど 1 サイトに束縛されている」場合だけ非ゼロ値になる。
			// 0 サイト（中央プロセス）では空のまま渡し、worker/watcher 側の既定の
			// site 未設定規約（空文字列 = db.DefaultSite に解決 / 定期ジョブ未登録）に
			// 委ねる。
			var boundSite config.MirakcSite
			if len(bound) == 1 {
				boundSite = bound[0]
			}
			// live はライブ視聴セッションごとに mirakc へアクセスするため、streamer
			// ロールが live.enabled で起動するにはちょうど 1 サイトへの束縛が要る
			// （watcher と同じ理由。issue #91 のライブ資源同定はこのタスクの対象外だが、
			// レジストリを導入した以上ここだけは壊さないための最小限の検査）。
			if cfg.Live.Enabled && slices.Contains(roles, "streamer") && len(bound) != 1 {
				return fmt.Errorf(
					"live.enabled requires exactly one bound site for the streamer role, got %d (use --sites <site>)",
					len(bound))
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// **1 発目を受けたらシグナルの登録を外す。** 外すまでの間、2 発目の
			// SIGTERM / SIGINT は signal パッケージのチャネル（バッファ 1・
			// 読み手はもう居ない）に落ちて捨てられ、既定動作（プロセス終了）が
			// 抑止されたままになる。1 発目のあとに続くのは River の drain で、
			// その長さは `--soft-stop-timeout` である --- encode を載せる構成には
			// 数時間を推奨しているので、**その間ずっと Ctrl-C も `kill -TERM` も
			// 効かない**（止める手段が SIGKILL だけになる）。
			//
			// **外す契機は「1 発目のシグナル」であって「畳み終えたところ」では
			// ない。** 後者にすると、drain を errgroup の中で回す 1 件消化モード
			// （stopOnceProcess）では畳み終えるまで外れず、**猶予を長く取る構成
			// ほど逃げ道が無くなる**（数時間の猶予を勧めている先が ScaledJob =
			// 1 件消化モードなので、一番効いてほしい構成で効かない）。
			//
			// 両モードとも TestServerCmd_SecondSigtermKillsDrainingProcess が
			// 固定する。
			context.AfterFunc(ctx, stop)

			pool, err := db.NewPool(ctx, cfg.DB, roles)
			if err != nil {
				return err
			}
			// **River の drain より後に閉じる。** defer の LIFO でそうなっている
			// （この defer より後に登録される stopRiverForShutdown が先に走る）。
			// 順序が逆になると、ctx を切られたジョブの結果を River の completer が
			// 書けず、行が `running` のまま残る --- この PR が避けようとしている
			// 形そのものである。
			defer pool.Close()

			slog.Info("starting server", "roles", roles)

			eg, egCtx := errgroup.WithContext(ctx)

			// HTTP リスナーはロールに関わらず 1 本立てる。ヘルスチェックと
			// /metrics はどのロールでも scrape できる必要があるため
			// （worker だけの Pod でも滞留メトリクスを取りたい）。
			// SPA・SSE・バイト配信は担当ロールのときだけ登録する。
			{
				backlog := newBoundBacklogCollector(pool, bound)
				routerCfg := api.RouterConfig{
					AllowedHosts:       cfg.Server.AllowedHosts,
					TrustForwardedHost: cfg.Server.TrustForwardedHost,
					Pool:               pool,
					// api は不変条件 1（mirakc にもファイルシステムにも依存しない）に
					// より site に束縛されない。boundSite ではなくレジストリ全体を渡す
					// ことで、1 プロセスがレジストリの全 site を処理できる
					// （issue #184 M4-12）。
					Sites:           registryNames(cfg.Registry()),
					MetricsRegistry: metrics.NewRegistry(backlog),
					// GET /api/capabilities に出すオプション機能（issue #209）。
					// フロントはこれを見てライブへの導線を出すかどうかを決める。
					//
					// **ロールで囲わない。** OpenAPI 生成ルート（HandlerFromMux）は
					// ロールに関わらず全プロセスに生えるので、api ロールの中だけで
					// 代入すると、同じ config の別プロセス（notifier 単独など）に
					// 聞いたときだけ live:false になる --- config → 公開面の値なのに
					// 答えがプロセスの役割で変わる。Sites / MetricsRegistry を
					// 無条件に渡しているのと同じ理由でここに置く（レビュー指摘）。
					//
					// 渡すのはこのプロセスが streamer ロールを持つかではなく config の
					// 値。ロール分割構成では api と streamer は別プロセスだが config
					// ファイルは共有する（docs/configuration.md §スキーマ構造）。
					LiveEnabled: cfg.Live.Enabled,
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

						// live.enabled かつ streamer ロールならちょうど 1 サイトへの束縛が
						// 保証済み（上の事前検査）なので boundSite を使ってよい。
						liveMirakcClient := mirakc.NewClient(boundSite.URL, nil)
						liveStreamer := streamer.NewLive(liveMirakcClient, boundSite.Site, convertLiveConfig(cfg.Live))
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

				srv := newProductionHTTPServer(cfg.Server.Listen, api.NewRouter(routerCfg))

				eg.Go(func() error {
					slog.Info("starting http server", "addr", cfg.Server.Listen)
					if httpErr := srv.ListenAndServe(); httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
						return fmt.Errorf("http server: %w", httpErr)
					}
					return nil
				})
				eg.Go(func() error {
					<-egCtx.Done()
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
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
				if worker.RequiresEncodeTools(queues) {
					if toolErr := cfg.Encode.ValidateTools(); toolErr != nil {
						return toolErr
					}
				}
				// worker.Deps.Site / worker.ClientConfig の各 *Site フィールドは
				// boundSite から取る（issue #183 の「含むもの」5）。0 サイト束縛
				// （中央プロセス）では boundSite がゼロ値になり、Site="" と
				// *Site="" が既存の慣習通りに解釈される（Deps.Site="" は
				// verifySite で db.DefaultSite に解決、ClientConfig の *Site="" は
				// その定期ジョブを登録しない。worker.ClientConfig のフィールド
				// コメント参照）。2 サイト以上の束縛では validateSiteBinding が
				// worker ロールを起動エラーにしているので、ここに来るのは
				// 0 または 1 サイト束縛のときだけ。
				mc := mirakc.NewClient(boundSite.URL, nil)
				workers := worker.NewWorkers(&worker.Deps{
					MirakcClient:             mc,
					Pool:                     pool,
					MediaDir:                 cfg.Storage.MediaDir,
					Site:                     boundSite.Site,
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
					// BoundSite は site 単位のキュー（ingest/epg/reconciler/watcher）を
					// 物理名（`<base>_<site>`）に展開するのに使う（issue #185 M4-13）。
					// boundSite.Site は 0 サイト束縛（中央プロセス）では空文字列のままで、
					// worker.qualifyQueueName が db.DefaultSite に解決する --- 0 サイト
					// 束縛の worker がこれらのキューを要求しないことは
					// validateSiteBinding が起動時に強制している。
					BoundSite:            boundSite.Site,
					IngestConcurrency:    cfg.Ingest.Concurrency,
					EncodeConcurrency:    cfg.Encode.Concurrency,
					ThumbnailConcurrency: cfg.Encode.ThumbnailConcurrency,
					EpgSyncInterval:      cfg.Epg.SyncInterval,
					PeriodicJobs:         cfg.Worker.PeriodicJobs,
					Queues:               queues,
					Once:                 onceGate,
					SoftStopTimeout:      softStopTimeout,
					// 定期ジョブ（epg_sync / tuner_sync / ruler_pass / reconcile_pass /
					// record_sweep / catalog_export / delete_reconcile /
					// encode_reconcile / storage_sync）は
					// worker 側が投入する（mirakc に触るのも各ジョブのヒント経路をまとめる
					// のも worker。riverClientFull は worker ロールがあるときにしか
					// 選ばれないので、ここは無条件に設定してよい）。
					EpgSyncSite:       boundSite.Site,
					TunerSyncSite:     boundSite.Site,
					RulerPassSite:     boundSite.Site,
					ReconcilePassSite: boundSite.Site,
					RecordSweepSite:   boundSite.Site,
					CatalogExport:     true,
					DeleteReconcile:   true,
					EncodeReconcile:   true,
					StorageSync:       true,
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
				// 1 件消化モードの購読は **Start より前**に張る（張る前に終わった
				// ジョブのイベントを取りこぼさないため）。middleware だけでは
				// 未登録 kind のジョブを観測できない（worker.SubscribeOnceEvents）。
				//
				// **worker ロールのガードの内側に置く。** River の Subscribe は
				// 「ジョブを実行しないクライアント」に対して panic するので
				// （river@v0.40.0/client.go の SubscribeConfig）、insert-only の
				// クライアント（watcher 単独）や nil のクライアントに対して呼ぶと
				// 起動エラーではなく panic になる。いまは validateOnceMode が
				// ロールを worker 単独に限っているので到達しないが、その検査の
				// 理由（常駐する役を巻き込まない）はクライアントの種類とは
				// 無関係なので、緩めたときにここが踏まれる形にしない。
				var onceEvents <-chan *river.Event
				if onceGate != nil {
					var unsubscribe func()
					onceEvents, unsubscribe = worker.SubscribeOnceEvents(riverClient)
					defer unsubscribe()
				}

				if startErr := riverClient.Start(ctx); startErr != nil {
					return fmt.Errorf("starting river client: %w", startErr)
				}
				defer func() { stopRiverForShutdown(riverClient.Stop, softStopTimeout) }()

				if onceGate != nil {
					// 1 件消化モードの終了契機。
					//
					// **停止の順序は stopOnceProcess が持つ**（graceful stop →
					// プロセス畳み。逆順にすると、実行中のジョブが完走に使える
					// 時間が soft stop の猶予まで縮む。River の Stop は StopInit
					// で冪等なので、上の defer の Stop はそのまま無害な no-op に
					// なる）。
					//
					// **ジョブが失敗しても nil を返す**（= exit 0）。リトライは
					// River が持っており、k8s 側は backoffLimit: 0 で起こし直さない
					// 前提なので、終了コードで失敗を表すと二重にリトライする形に
					// なる（worker.ClientConfig.Once のコメント）。
					//
					// 既知の窓（実害が空振り Job 1 つぶんなので閉じていない）:
					// Work を抜けてから Stop が producer を止めるまでの間に次の
					// ジョブが fetch されると、この Job は 2 件消化して終わる。
					// MaxWorkers=1 なので同時には走らない。**打ち切られるのは
					// `--soft-stop-timeout` の猶予を超えたときだけ**である
					// （実測 0/25 でこの窓には入らなかった）。
					eg.Go(func() error {
						outcome := onceGate.Wait(egCtx, onceIdleTimeout, onceEvents)
						slog.Info("worker: once mode finished", "outcome", outcome.String())
						stopOnceProcess(ctx, riverClient.Stop, stop)
						return nil
					})
				}
			}

			// シングルトンロールは監督ループで管理する。
			// ロック取得を定期試行し、取得後は heartbeat で監視する。
			for _, r := range roles {
				if !slices.Contains(singletonRoles, r) {
					continue
				}
				roleName := r
				// lockName は advisory lock のキー（role.RunSingleton の第 3 引数）。
				// watcher だけは site で修飾する（watcherLockName、issue #185 M4-13
				// 「含むもの」8）。修飾しないと、多サイト構成で 2 つの watcher が
				// 同じロックを取り合い、負けた側の mirakc の SSE を誰も購読しなくなる
				// （issue #183 本文。負けた側は "role already held by another process"
				// を出して 15 秒ごとにポーリングし続けるだけなので、ログ上は正常に見える）。
				lockName := roleName
				eg.Go(func() error {
					roleFunc := func(ctx context.Context) error {
						slog.Info("role started", "role", roleName)
						<-ctx.Done()
						slog.Info("role stopped", "role", roleName)
						return ctx.Err()
					}
					switch roleName {
					case "watcher":
						// validateSiteBinding が watcher ロールを常にちょうど 1 サイト
						// 束縛に限定しているので、ここでは boundSite が必ず埋まっている。
						mc := mirakc.NewClient(boundSite.URL, nil)
						w := watcher.New(boundSite.Site, mc, pool, riverClient, worker.NewIngestArgs, webhookClient)
						roleFunc = w.Run
						lockName = watcherLockName(boundSite.Site)
					}
					return role.RunSingleton(egCtx, pool, lockName, roleFunc, nil)
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
	// --sites はプロセスの束縛を表す起動形態フラグで、config キーにしない
	// （resolveSiteBinding のコメント、docs/configuration.md §やらないこと）。
	// 未指定（デフォルト値 nil）と `--sites=`（空スライス）は
	// cmd.Flags().Changed で区別する。
	cmd.Flags().StringSlice(siteFlagName, nil,
		"mirakc sites this process is bound to (comma-separated). "+
			"Omit to bind to the registry's sole entry; pass an empty value to run unbound (central process)")
	// --queues / --once も --sites と同じ「起動形態」の軸（resolveWorkerQueues /
	// onceFlagName のコメント）。--queues は config の worker.queues と
	// 排他で、両方指定は起動エラーになる。
	cmd.Flags().StringSlice(queuesFlagName, nil,
		"queues the worker role pulls (comma-separated logical names). "+
			"Mutually exclusive with worker.queues in the config file")
	cmd.Flags().Bool(onceFlagName, false,
		"exit after working a single job (for KEDA ScaledJob; requires --roles worker, "+
			"exactly one queue and worker.periodic_jobs: false)")
	cmd.Flags().Duration(onceIdleTimeoutFlagName, defaultOnceIdleTimeout,
		"in --once mode, exit successfully if no job is claimed within this duration "+
			"(never applies once a job is running)")
	// --soft-stop-timeout も同じ「起動形態」の軸（softStopTimeoutFlagName の
	// コメント）。対になる k8s の terminationGracePeriodSeconds は
	// shutdownBudget を包む長さにする。
	cmd.Flags().Duration(softStopTimeoutFlagName, worker.DefaultSoftStopTimeout,
		"how long a running job may keep going after SIGTERM before its context is cancelled "+
			"(the pod's terminationGracePeriodSeconds must cover this)")

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
	// 同じ名前の重複は 1 つに畳む（`--sites tokyo,tokyo` /
	// `--queues ingest,ingest` と揃える）。畳まないと
	// `--roles worker,worker --once` が「ちょうど worker 1 つ」の検査に
	// 引っかかり、「requires exactly the worker role, got [worker, worker]」
	// という紛らわしいエラーになる。
	//
	// プール上限（db.maxConnsForRoles）は元から uniqueRoles で畳んでいるので、
	// そちらは以前も二重計上していない。
	deduped := make([]string, 0, len(roles))
	seen := make(map[string]bool, len(roles))
	for _, r := range roles {
		if !slices.Contains(allRoles, r) {
			return nil, fmt.Errorf("unknown role: %q (valid: %s)", r, strings.Join(allRoles, ", "))
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		deduped = append(deduped, r)
	}
	return deduped, nil
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

// watcherLockName は watcher ロールの pg_advisory_lock キー（role.RunSingleton
// の第 3 引数）を site で修飾した名前を返す（issue #185 M4-13「含むもの」8）。
//
// site を含めないと、多サイト構成で 2 つの watcher プロセスが同じロックを
// 取り合い、負けた側の mirakc の SSE を誰も購読しなくなる（issue #183 本文）。
// validateSiteBinding が watcher ロールを常にちょうど 1 サイト束縛に限定して
// いるので、呼び出し側で site が空文字列になることはない。
func watcherLockName(site string) string {
	return "watcher:" + site
}
