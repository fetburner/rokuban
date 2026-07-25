package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewRouter は API エンドポイントと SPA 配信を統合した http.Handler を返す。
// distFS が nil の場合は API のみ（テスト用）。
// hub が nil の場合は SSE エンドポイントを登録しない。
func NewRouter(allowedHosts []string, distFS fs.FS, pool *pgxpool.Pool, hub *EventHub) http.Handler {
	r := chi.NewRouter()

	r.Use(AllowedHosts(allowedHosts))

	// SSE は長寿命ストリームで OpenAPI のリクエスト/レスポンスモデルに乗らないため、
	// 生成ハンドラを通さず直接登録する。仕様は docs/api.md 側に置く。
	if hub != nil {
		r.Get("/api/events", eventsHandler(hub))
	}

	handler := NewServer(pool)
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	if distFS != nil {
		r.NotFound(NewSPAHandler(distFS).ServeHTTP)
	}

	return r
}
