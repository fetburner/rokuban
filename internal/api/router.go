package api

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter は API エンドポイントと SPA 配信を統合した http.Handler を返す。
// distFS が nil の場合は API のみ（テスト用）。
func NewRouter(allowedHosts []string, distFS fs.FS) http.Handler {
	r := chi.NewRouter()

	// X-Forwarded-For のクライアント IP 復元はデプロイ構成依存のため、
	// 必要時に middleware.ClientIPFromXFF / ClientIPFromXFFTrustedProxies を設定する
	r.Use(AllowedHosts(allowedHosts))

	handler := &Server{}
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	if distFS != nil {
		r.NotFound(NewSPAHandler(distFS).ServeHTTP)
	}

	return r
}
