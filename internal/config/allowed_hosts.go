package config

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
)

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

// validateInternalURLHosts は、server.allowed_hosts が空でないとき、各置き場の internal_url の
// ホストがその一覧にあることを確かめる。転送の worker は internal_url の streamer へ直接要求するので、
// 一覧に無いホストは streamer の Host 検証で 400 になり、転送が必ず失敗する
// （kind の 2 置き場 e2e の判定 6.3 で実測）。起動時に止めて設定漏れを先に見せる。
//
// ponytail: api が常に許す localhost 系は特別扱いしない。別 Pod を指す internal_url に
// localhost を書く構成は無いので、要るなら allowed_hosts に足せばよい。
func validateInternalURLHosts(allowedHosts []string, stores map[string]StoreConfig) []string {
	if len(allowedHosts) == 0 {
		return nil
	}
	allowed := make([]string, len(allowedHosts))
	for i, h := range allowedHosts {
		allowed[i] = NormalizeHost(h)
	}
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	for _, name := range names {
		raw := stores[name].InternalURL
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue // URL の形は validateStorageURL が報告する
		}
		if host := NormalizeHost(u.Hostname()); !slices.Contains(allowed, host) {
			errs = append(errs, fmt.Sprintf("storage.stores.%s.internal_url host %q is not in server.allowed_hosts; the streamer behind it rejects other hosts with 400, so transfers from this store would always fail", name, host))
		}
	}
	return errs
}
