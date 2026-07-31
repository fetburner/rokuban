package api

import (
	"net"
	"net/http"
	"slices"
)

// alwaysAllowedHosts は allowlist の設定に関わらず許可する Host。
// ローカルからのアクセスは DNS rebinding の経路にならない
// （docs/configuration.md が「localhost 系は常に許可」と定めている）。
var alwaysAllowedHosts = []string{"localhost", "127.0.0.1", "::1", "[::1]"}

// infraPaths は Host 検証を免除するパス。
//
// ヘルスチェックと metrics は監視基盤が Pod IP やサービス名で叩くため、
// allowlist に載せようがない（IP は動的）。allowlist の内側に置くと
// k8s の liveness probe と Prometheus の scrape が 400 で落ちる。
//
// DNS rebinding が守ろうとしているのはブラウザ経由でデータを読み書きされる
// ことなので、機密を含まないインフラ用エンドポイントを免除しても防壁は薄くならない。
var infraPaths = map[string]bool{
	"/healthz": true,
	"/metrics": true,
}

// AllowedHosts は DNS rebinding 対策として Host ヘッダーを allowlist で検証する。
// 認証を持たない LAN アプリでは、悪意あるサイトが DNS rebinding 経由で
// ブラウザから API を叩ける攻撃を防ぐ唯一の防壁になる。
// allowedHosts が空の場合はチェックをスキップする（開発用）。
//
// リバースプロキシ前段では `Host` がプロキシ自身の値に書き換わり、元の
// クライアント向け Host は `X-Forwarded-Host` に移る（docs/api.md §リバース
// プロキシ・フレンドリー要件、issue #89）。そのため `X-Forwarded-Host` が
// あればそちらを検証対象にし、無ければ従来通り `r.Host` を使う。
func AllowedHosts(allowedHosts []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(allowedHosts) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if infraPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			host := stripPort(forwardedHost(r))
			if !slices.Contains(allowedHosts, host) && !slices.Contains(alwaysAllowedHosts, host) {
				http.Error(w, "invalid host", http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedHost は Host allowlist 検証に使う Host 値を返す。
// `X-Forwarded-Host` があればそちらを、無ければ `r.Host` を返す。
func forwardedHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return h
	}
	return r.Host
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}
