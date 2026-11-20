// Package hostname は Host の比較規則を置く。Host allowlist（server.allowed_hosts）の
// 各要素と、受け取った Host を同じ形に揃えてから比べるための正規化を定める。
// 正規化は ASCII の小文字化と末尾ドット 1 個の除去だけで、他の値の変換はしない。
package hostname

// NormalizeHost は Host allowlist（server.allowed_hosts）の比較に使う正規化を行う。
// api の Host 検証と internal_url の起動時検証が同じ規則で比べるために、ここに置く。ASCII 範囲だけの
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
func NormalizeHost(host string) string {
	if n := len(host); n >= 2 && host[n-1] == '.' {
		host = host[:n-1]
	}
	b := []byte(host)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
