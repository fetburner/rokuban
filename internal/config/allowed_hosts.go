package config

import (
	"fmt"
	"net/url"
	"slices"
	"sort"

	"github.com/fetburner/rokuban/internal/hostname"
)

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
		allowed[i] = hostname.NormalizeHost(h)
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
		if host := hostname.NormalizeHost(u.Hostname()); !slices.Contains(allowed, host) {
			errs = append(errs, fmt.Sprintf("storage.stores.%s.internal_url host %q is not in server.allowed_hosts; the streamer behind it rejects other hosts with 400, so transfers from this store would always fail", name, host))
		}
	}
	return errs
}
