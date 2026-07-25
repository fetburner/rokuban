package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RouterConfig は NewRouter の設定。
// nil / ゼロ値のフィールドはそのロールを無効にする（テストで部分構成を作れる）。
type RouterConfig struct {
	// AllowedHosts は Host ヘッダーの allowlist。空なら検査しない。
	AllowedHosts []string

	// Pool は REST ハンドラが使う DB プール。
	Pool *pgxpool.Pool

	// DistFS が非 nil なら SPA を配信する。
	DistFS fs.FS

	// Hub が非 nil なら SSE (/api/events) を配信する。
	Hub *EventHub

	// Mounter が非 nil なら追加のルートを登録する。
	// バイト配信（streamer）を api の外に置いたまま同一プロセスで
	// 相乗りさせるための口（不変条件 1）。
	Mounter interface{ Mount(chi.Router) }
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

	handler := NewServer(cfg.Pool)
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	if cfg.DistFS != nil {
		r.NotFound(NewSPAHandler(cfg.DistFS).ServeHTTP)
	}

	return r
}
