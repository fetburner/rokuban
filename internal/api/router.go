package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riverqueue/river"
)

// RouterConfig は NewRouter の設定。
// nil / ゼロ値のフィールドはそのロールを無効にする（テストで部分構成を作れる）。
type RouterConfig struct {
	// AllowedHosts は Host ヘッダーの allowlist。空なら検査しない。
	AllowedHosts []string

	// Pool は REST ハンドラが使う DB プール。
	Pool *pgxpool.Pool

	// RiverClient が非 nil なら、ルール作成/更新/削除のヒントで RulerPassArgs を
	// 同一トランザクションで投入する（InsertTx。dual-write を避けるため。
	// docs/recording.md §3.1「ヒント」）。insert-only で足り、api が worker の
	// ワーカー群を登録することはない（不変条件: api は mirakc に問い合わせず
	// ffmpeg も実行しない）。nil なら投入しない（テストや将来のサーバーレス構成で
	// River を持たない api を許容するため）。
	RiverClient *river.Client[pgx5.Tx]

	// DistFS が非 nil なら SPA を配信する。
	DistFS fs.FS

	// Hub が非 nil なら SSE (/api/events) を配信する。
	Hub *EventHub

	// Mounter が非 nil なら追加のルートを登録する。
	// バイト配信（streamer）を api の外に置いたまま同一プロセスで
	// 相乗りさせるための口（不変条件 1）。
	Mounter interface{ Mount(chi.Router) }

	// MetricsRegistry が非 nil なら /metrics で Prometheus メトリクスを公開する。
	MetricsRegistry *prometheus.Registry
}

// NewRouter は API エンドポイントと SPA 配信を統合した http.Handler を返す。
func NewRouter(cfg RouterConfig) http.Handler {
	r := chi.NewRouter()

	r.Use(AllowedHosts(cfg.AllowedHosts))

	// SSE は長寿命ストリームで OpenAPI のリクエスト/レスポンスモデルに乗らないため、
	// 生成ハンドラを通さず直接登録する。仕様は docs/api.md 側に置く。
	if cfg.Hub != nil {
		r.Get("/api/events", eventsHandler(cfg.Hub))
	}

	// バイト配信も同様に OpenAPI に載せない（生成クライアントは JSON を前提にする）。
	if cfg.Mounter != nil {
		cfg.Mounter.Mount(r)
	}

	// /metrics は Prometheus の text exposition format で、OpenAPI の対象外。
	// /api/ の下に置かないのは慣習（scrape 設定やリバースプロキシの除外を書きやすい）。
	if cfg.MetricsRegistry != nil {
		r.Handle("/metrics", promhttp.HandlerFor(cfg.MetricsRegistry, promhttp.HandlerOpts{
			// scrape 中のコレクタのエラーはログに出し、500 にはしない。
			// 一部のメトリクスが取れなくても残りは配る。
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	handler := NewServer(cfg.Pool, cfg.RiverClient)
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	if cfg.DistFS != nil {
		r.NotFound(NewSPAHandler(cfg.DistFS).ServeHTTP)
	}

	return r
}
