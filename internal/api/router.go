package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func NewRouter(allowedHosts []string) http.Handler {
	r := chi.NewRouter()

	// X-Forwarded-For のクライアント IP 復元はデプロイ構成依存のため、
	// 必要時に middleware.ClientIPFromXFF / ClientIPFromXFFTrustedProxies を設定する
	r.Use(AllowedHosts(allowedHosts))

	handler := &Server{}
	strict := NewStrictHandler(handler, nil)
	HandlerFromMux(strict, r)

	return r
}
