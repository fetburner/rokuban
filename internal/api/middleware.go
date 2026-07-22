package api

import (
	"net"
	"net/http"
	"slices"
)

// AllowedHosts は DNS rebinding 対策として Host ヘッダーを allowlist で検証する。
// 認証を持たない LAN アプリでは、悪意あるサイトが DNS rebinding 経由で
// ブラウザから API を叩ける攻撃を防ぐ唯一の防壁になる。
// allowedHosts が空の場合はチェックをスキップする（開発用）。
func AllowedHosts(allowedHosts []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(allowedHosts) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := stripPort(r.Host)
			if !slices.Contains(allowedHosts, host) {
				http.Error(w, "invalid host", http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}
