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

	// Sites はこのプロセスが応答してよい mirakc サイト名の一覧
	// （config.mirakc/mirakcs レジストリが権威）。空なら db.DefaultSite の
	// 1 要素とみなす（テストの部分構成を許す）。api は不変条件 1 によりどの
	// site にも束縛されないため、1 プロセスがレジストリの全 site を処理できる
	// （issue #184 M4-12）。
	Sites []string

	// RiverClient が非 nil なら、ルール作成/更新/削除のヒントで RulerPassArgs を
	// 同一トランザクションで投入する（InsertTx。dual-write を避けるため。
	// docs/recording.md §3.1「ヒント」）。insert-only で足り、api が worker の
	// ワーカー群を登録することはない（不変条件: api は mirakc に問い合わせず
	// ffmpeg も実行しない）。nil なら投入しない（テストや将来のサーバーレス構成で
	// River を持たない api を許容するため）。
	RiverClient *river.Client[pgx5.Tx]

	// DistFS が非 nil なら SPA を配信する。
	DistFS fs.FS

	// Mounter が非 nil なら追加のルートを登録する。
	// バイト配信（streamer）・SSE 配信（notifier）を api の外に置いたまま同一
	// プロセスで相乗りさせるための口（不変条件 1）。api ロール単独では常に nil
	// なので、/api/events は生えない（404 になる。M2-19）。
	//
	// 複数のロールを同居させたい場合（monolith / --all）は Mounters で束ねる。
	Mounter Mounter

	// MetricsRegistry が非 nil なら /metrics で Prometheus メトリクスを公開する。
	MetricsRegistry *prometheus.Registry

	// EncodeProfileNames は config.encode.profiles の名前一覧。
	// ルール create/update と予約 overrides で encodeProfiles に未知名があれば 400 にする
	// （issue #64）。空/nil なら名前検証をスキップする（テストの部分構成を許す）。
	EncodeProfileNames []string
}

// Mounter はルーターへ追加のルートを登録する。
// バイト配信（streamer）・SSE 配信（notifier）のように、OpenAPI 生成ルートの
// 外側にエンドポイントを持つロールを api と同一プロセスに同居させるための
// 口（不変条件 1）。api 自身はこのインタフェースの実装を持たない —
// mirakc にもファイルシステムにも依存しない純粋なリクエスト/レスポンス層に
// 留めるため（M2-19）。
type Mounter interface {
	Mount(chi.Router)
}

// Mounters は複数の Mounter を 1 つの Mounter にまとめる。
//
// RouterConfig.Mounter は 1 つしか受け取れないが、monolith（--all）では
// streamer と notifier の両方を同一プロセス・同一リスナーに同居させたい。
// スライスにして Mount で順に呼ぶだけの薄いアダプタにすることで、
// RouterConfig の型もフィールド数も増やさずに済む —
// 単一の Mounter しか要らない既存の呼び出し（テスト・単一ロール構成）は
// 一切変更が要らない。複数を束ねたいときだけ Mounters{a, b} を渡す。
type Mounters []Mounter

// Mount は束ねたすべての Mounter を登録順に呼ぶ。
func (ms Mounters) Mount(r chi.Router) {
	for _, m := range ms {
		m.Mount(r)
	}
}

// NewRouter は API エンドポイントと SPA 配信を統合した http.Handler を返す。
func NewRouter(cfg RouterConfig) http.Handler {
	r := chi.NewRouter()

	r.Use(AllowedHosts(cfg.AllowedHosts))

	// SSE (/api/events) やバイト配信は OpenAPI に載せない（生成クライアントは
	// JSON を前提にする）ため、生成ハンドラを通さず Mounter 経由で直接登録する。
	// 仕様は docs/api.md 側に置く。
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

	handler := NewServer(cfg.Pool, cfg.RiverClient, cfg.Sites, cfg.EncodeProfileNames)
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	if cfg.DistFS != nil {
		r.NotFound(NewSPAHandler(cfg.DistFS).ServeHTTP)
	}

	return r
}
