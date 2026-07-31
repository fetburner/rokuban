package api

import (
	"net"
	"net/http"
	"slices"
	"strings"
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
//
// `alwaysAllowedHosts`（localhost 系の常時許可）は `r.Host` を直接使う
// ときにのみ適用する。`X-Forwarded-Host` は自己申告値（クライアントや
// 途中の何者かが書ける）なので、`localhost` を騙って allowlist を素通り
// できてしまう（実際の TCP 接続相手が localhost であることが前提の緩和
// なので、転送された値には適用できない）。
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
			forwarded, isForwarded := forwardedHost(r)
			var host string
			if isForwarded {
				host = stripPort(forwarded)
			} else {
				host = stripPort(r.Host)
			}
			allowed := slices.Contains(allowedHosts, host)
			if !isForwarded {
				allowed = allowed || slices.Contains(alwaysAllowedHosts, host)
			}
			if !allowed {
				http.Error(w, "invalid host", http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedHost は `X-Forwarded-Host` ヘッダーから Host allowlist 検証に
// 使う値を取り出す。ヘッダーが無ければ ("", false) を返す。
//
// 複数プロキシを経由すると `X-Forwarded-Host` は `X-Forwarded-For` と
// 同様、各ホップが自分の見た値を追記してカンマ区切り・複数ヘッダー行に
// なりうる。検証すべきは「クライアントに最も近い、最初にプロキシを
// 通った時点の値」なので、複数行（`r.Header.Values`。テキストとして
// 連結した1つの値と等価で、ワイヤ上の出現順を保つ）の最初の行の、さらに
// 先頭カンマ区切り要素を使う。
func forwardedHost(r *http.Request) (string, bool) {
	values := r.Header.Values("X-Forwarded-Host")
	if len(values) == 0 {
		return "", false
	}
	first := values[0]
	if i := strings.IndexByte(first, ','); i >= 0 {
		first = first[:i]
	}
	return strings.TrimSpace(first), true
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}
