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
// プロキシ・フレンドリー要件、issue #89）。ただし `X-Forwarded-Host` は
// クライアントが自由に送れるヘッダーであり、前段にプロキシが存在しない
// 直接露出構成（`--all` の既定構成）でこの自己申告をそのまま allowlist の
// 判定に使うと、DNS rebinding 攻撃ページが `X-Forwarded-Host: <allowlist に
// 載っている値>` を自己申告するだけで検証を素通りできてしまう（issue #216）。
// そのため `trustForwardedHost` が true（`server.trust_forwarded_host` を
// 明示した、信頼できるプロキシが必ず前段に居る構成）のときにだけ
// `X-Forwarded-Host` を検証対象にし、それ以外（既定 = false）では常に
// `r.Host` を使う。
//
// `alwaysAllowedHosts`（localhost 系の常時許可）は `r.Host` を直接使う
// ときにのみ適用する。`X-Forwarded-Host` は自己申告値（クライアントや
// 途中の何者かが書ける）なので、`localhost` を騙って allowlist を素通り
// できてしまう（実際の TCP 接続相手が localhost であることが前提の緩和
// なので、転送された値には適用できない）。
//
// リクエストラインが absolute-form（`GET http://host/path HTTP/1.1`）だと
// `net/http` は `r.URL.Host` を `r.Host` として採用し、`Host` ヘッダーは
// 一致するか否かに関わらず常に `r.Header` から削除する（`net/http` の
// `readRequest`）。そのためハンドラ側から「リクエストラインと `Host`
// ヘッダーが食い違うか」を見分ける手段は無く、`r.Header.Get("Host")` は
// サーバー側では常に空文字になる。この allowlist を「`Host` ヘッダーの
// 検証」だと言い切るには、absolute-form 自体を拒否するしかない。
// origin サーバーに absolute-form を送るのはプロキシだけで、ブラウザ・
// `curl`（プロキシ未経由）は origin-form しか送らないため、拒否しても
// 壊れる正当な利用者は居ない。
func AllowedHosts(allowedHosts []string, trustForwardedHost bool) func(http.Handler) http.Handler {
	normalizedAllowedHosts := make([]string, len(allowedHosts))
	for i, h := range allowedHosts {
		normalizedAllowedHosts[i] = normalizeHost(h)
	}
	return func(next http.Handler) http.Handler {
		if len(normalizedAllowedHosts) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if infraPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			if r.URL.Host != "" {
				http.Error(w, "invalid host", http.StatusBadRequest)
				return
			}
			var forwarded string
			var isForwarded bool
			if trustForwardedHost {
				forwarded, isForwarded = forwardedHost(r)
			}
			var host string
			if isForwarded {
				host = normalizeHost(stripPort(forwarded))
			} else {
				host = normalizeHost(stripPort(r.Host))
			}
			allowed := slices.Contains(normalizedAllowedHosts, host)
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

// normalizeHost は Host allowlist の比較に使う正規化を行う。ASCII 範囲だけの
// 小文字化と、末尾ドット 1 個の除去だけを行う。
//
// Unicode の case folding（`strings.ToLower`）は使わない。fail-open になり
// うるためで、たとえば `strings.ToLower("K")`（ケルビン記号）は `"k"`
// を返すので、非 ASCII の `Host` が `allowed_hosts` の ASCII ホスト名に
// 一致してしまう。ホスト名の case-insensitive は ASCII の範囲の規則
// （RFC 4343）なので、正規化も ASCII に限る。
//
// 末尾ドットは DNS 上 `rokuban.local.` と `rokuban.local` が同じ名前で
// あることに対応する（絶対 FQDN 表記の利用者や、一部プロキシがそのまま
// `Host` に載せてくる）。落とすのは 1 個だけで、`..` のような不正な形は
// 正規化しない（DNS 名として無効なので通す理由が無い）。
func normalizeHost(host string) string {
	trimmed := host
	if n := len(trimmed); n >= 2 && trimmed[n-1] == '.' {
		trimmed = trimmed[:n-1]
	}
	needsLower := false
	for i := 0; i < len(trimmed); i++ {
		if c := trimmed[i]; c >= 'A' && c <= 'Z' {
			needsLower = true
			break
		}
	}
	if !needsLower {
		return trimmed
	}
	b := []byte(trimmed)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
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

// stripPort は "host:port" 形式の入力から port 部分を取り除く。
//
// `net.SplitHostPort` はコロンの位置だけで分解し、port 部分が数値である
// ことを検証しない。そのため `rokuban.local:evil.com` のような入力は
// host="rokuban.local" / port="evil.com" に分解されてしまい、
// allowlist の比較に host だけを使うと `evil.com` が黙って切り落とされる
// （fail-open）。port が数値でない場合は host/port の分解自体が無効と
// みなし、**入力をそのまま返す**（fail-closed）。呼び出し元の allowlist
// 比較では "rokuban.local:evil.com" 全体が比較対象になり、一致せず拒否
// される。
//
// この関数は `[::1]` のように port を持たない入力（`net.SplitHostPort`
// がエラーを返す）でも入力をそのまま返す。`alwaysAllowedHosts` に
// `"::1"` と `"[::1]"` の両方を含めているのはこの振る舞いに対応する
// ためで、ここを変えるときは `TestAllowedHosts_LocalhostAlwaysAllowed`
// の `[::1]:40773` サブテストが崩れないことを確認する。
//
// 空ポート（`"rokuban.local:"`）も isNumericPort が false を返すため、
// 数値でないポートと同じ fail-closed 側に落ちる（入力をそのまま返し、
// 呼び出し元の allowlist 比較で不一致になり拒否される）。RFC 9110 の
// `port = *DIGIT` は空ポートを構文上有効（既定ポートの意）としているが、
// 空ポートだけを許可側に戻す特別扱いは isNumericPort に分岐を増やす
// だけで、それで救う正当な利用者がいるかは未検証。分岐を増やさない側
// （他の非数値ポートと同じ拒否）に倒している。
func stripPort(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || !isNumericPort(port) {
		return hostport
	}
	return host
}

// isNumericPort は s が 1 文字以上の数字だけで構成されるかを判定する。
// `net.SplitHostPort` は port 部分の内容を検証しないため、この関数が
// stripPort の fail-closed 判定を担う。
func isNumericPort(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
