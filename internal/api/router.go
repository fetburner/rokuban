package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewRouter は API エンドポイントと SPA 配信を統合した http.Handler を返す。
// distFS が nil の場合は API のみ（テスト用）。
func NewRouter(allowedHosts []string, distFS fs.FS, pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()

	r.Use(AllowedHosts(allowedHosts))

	handler := NewServer(pool)
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	if distFS != nil {
		r.NotFound(NewSPAHandler(distFS).ServeHTTP)
	}

	return r
}
