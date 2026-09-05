package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
)

// siteFlagName は `server` サブコマンドのプロセス束縛フラグ名。config キーには
// しない（docs/configuration.md §やらないこと「CLI フラグで設定値を渡すこと」に
// 抵触しない --- これは設定値ではなく `--all`/`--roles` と同じ「起動形態」の軸。
// issue #183 M4-11）。
const siteFlagName = "sites"

// resolveSiteBinding は `--sites` フラグと mirakc レジストリから、このプロセスが
// 束縛されるサイトの一覧を返す。
//
// 解決規則（issue #183 の「含むもの」4）:
//   - フラグ未指定（cmd.Flags().Changed(siteFlagName) == false）かつレジストリが
//     1 要素 → その 1 つに束縛する
//   - フラグ未指定かつレジストリが 2 要素以上 → 起動エラー（暗黙に「全部」にしない）
//   - `--sites=`（明示的な空文字列。pflag の StringSlice は空文字列を長さ 0 の
//     スライスとして扱う）→ 束縛なし（中央プロセス）
//   - `--sites tokyo` 等 → 指定された名前がレジストリに存在する場合に限りそれらに束縛
//     （同じ名前の重複指定は 1 つに畳む。`--sites tokyo,tokyo` が「2 サイト束縛」
//     という紛らわしいエラーにならないようにするため）
//
// **「未指定」と「明示的な空」を区別する必要がある**（issue #183 の「罠」）。
// pflag の StringSlice はどちらも長さ 0 のスライスになるため、Changed() で判定する。
func resolveSiteBinding(cmd *cobra.Command, registry []config.MirakcSite) ([]config.MirakcSite, error) {
	names, err := cmd.Flags().GetStringSlice(siteFlagName)
	if err != nil {
		return nil, err
	}

	if !cmd.Flags().Changed(siteFlagName) {
		switch len(registry) {
		case 0:
			// validateMirakcRegistry が Load 時点で弾いているはずなので、
			// ここに来るのは Config を手で組み立てたテスト等に限る。
			return nil, fmt.Errorf("mirakc registry is empty")
		case 1:
			return registry, nil
		default:
			return nil, fmt.Errorf(
				"--sites is required: mirakcs has %d entries and binding to all of them implicitly is not supported",
				len(registry))
		}
	}

	if len(names) == 0 {
		// --sites=（明示的な空）: 束縛なし = 中央プロセス
		return nil, nil
	}

	byName := make(map[string]config.MirakcSite, len(registry))
	for _, s := range registry {
		byName[s.Site] = s
	}

	// 同じ名前が複数回渡された場合は 1 つに畳む（`--sites tokyo,tokyo` が
	// 「2 サイト束縛」という紛らわしいエラーにならないようにするため。
	// issue #183 のレビュー指摘）。
	bound := make([]config.MirakcSite, 0, len(names))
	seen := make(map[string]bool, len(names))
	var unknown []string
	for _, n := range names {
		s, ok := byName[n]
		if !ok {
			unknown = append(unknown, n)
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		bound = append(bound, s)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--sites: unknown site(s) %s (registry has: %s)",
			strings.Join(unknown, ", "), strings.Join(registryNames(registry), ", "))
	}
	return bound, nil
}

func registryNames(registry []config.MirakcSite) []string {
	names := make([]string, 0, len(registry))
	for _, s := range registry {
		names = append(names, s.Site)
	}
	return names
}

// validateSiteBinding は束縛サイト数・ロール集合・worker.queues の組み合わせを
// 検査する。
//
// **watcher** と **worker** はどちらも 1 プロセスが N site を束縛できる
// （issue #532。watcher は site ごとに goroutine + advisory lock を持ち
// （server.go、watcherLockName）、worker は Deps.MirakcClients /
// ClientConfig.BoundSites が site → 値の map / 集合になったため）。
// そのため、束縛サイト数そのものに対する上限・下限の検査はここには無い ---
// 残るのは次の 1 つだけ:
//
// **worker が 0 サイト（中央プロセス）で site 束縛キューを要求する構成は
// 起動エラーのまま**。`jobs.RequiresSiteBinding` が true（`worker.queues` が
// 空、または ingest/epg/reconciler/watcher のいずれかを含む）なら、届く
// site 単位のジョブは Deps.MirakcClients が空 map のため verifySite が必ず
// 拒み、全滅して再試行し続けるだけの構成になる。0 サイトの worker を許すのは
// `worker.queues` を ruler/encode/thumbnail/cleanup/storage 等の site 非依存
// キューに絞ったときだけ（BoundSites が空のままでも、これらのキューの
// ジョブは site 単位の照合を必要としない。worker.ClientConfig の
// フィールドコメント参照）。
func validateSiteBinding(roles []string, bound []config.MirakcSite, queues []string) error {
	if slices.Contains(roles, "worker") && len(bound) == 0 && jobs.RequiresSiteBinding(queues) {
		return fmt.Errorf("--sites: worker role is unbound (central process) but worker.queues %v "+
			"still includes site-bound queues (or is empty, meaning all queues); "+
			"restrict worker.queues to site-independent queues (ruler/encode/thumbnail/cleanup/storage/default --- "+
			"catalog_export and delete_reconcile ride the cleanup queue, storage_sync rides the storage queue) "+
			"or bind to at least one site with --sites (issue #185)", queues)
	}
	return nil
}

// requireSingleSite は registry がちょうど 1 要素であることを要求する。
//
// `import epgstation` が単一サイト用のまま使う（issue #533 で rescue /
// shadow-diff は resolveSiteFlag に置き換わったので、この関数の利用者は
// import だけになった）: 多サイトでの意味論（EPGStation からの移行先を
// どう決めるか）を決める書き手がまだいない（不変条件 11）ので、形を決めずに
// 明示的なエラーで落とす。
func requireSingleSite(registry []config.MirakcSite, cmdName string) (config.MirakcSite, error) {
	switch len(registry) {
	case 0:
		return config.MirakcSite{}, fmt.Errorf("mirakc registry is empty")
	case 1:
		return registry[0], nil
	default:
		return config.MirakcSite{}, fmt.Errorf(
			"%s: multi-site config (mirakcs has %d entries) is not supported yet; "+
				"the EPGStation-side site mapping has no writer yet",
			cmdName, len(registry))
	}
}

// newBoundBacklogCollectors は束縛サイトごとに 1 つずつ metrics.BacklogCollector
// を作る（issue #532: 旧「ちょうど 1 サイト」の制限を N サイトへ一般化した）。
//
// BacklogCollector は「このサイトで未 ingest の record がどれだけ滞留しているか」
// という site 束縛の観測（internal/metrics/backlog.go）。0 サイト（中央プロセス）
// では「このサイト」が定まらないので空スライスを返す --- 登録すると担当していない
// サイトの系列を出してしまう（issue #183 の受け入れ基準）。**2 サイト以上でも
// 「どちらの site か定まらない」から登録しない、という旧判断は誤りだった**:
// N サイト束縛のちょうど N 個のコレクタを作れば、系列はそれぞれ自分の site
// ラベルを持つので曖昧にならない（issue #532 のレビュー指摘。takamatsu の
// ような WAN 越し site ほど ingest backlog の監視が要る）。
//
// **戻り値の要素は常に具体的に構築したコレクタで、nil を混ぜない。** 「具体型 nil
// を interface に入れると nil でなくなる」という Go の罠（internal/metrics.NewRegistry
// の doc コメント参照。`--sites=` で起動した実バイナリが起動時 panic した実例）は
// ここでは「0 個の要素を持つスライスを返す」ことで避ける --- 個々の要素を nil に
// する分岐を一切持たない。
func newBoundBacklogCollectors(pool *pgxpool.Pool, bound []config.MirakcSite) []prometheus.Collector {
	collectors := make([]prometheus.Collector, 0, len(bound))
	for _, s := range bound {
		collectors = append(collectors, metrics.NewBacklogCollector(pool, s.Site))
	}
	return collectors
}

// resolveSiteFlag は `--site` フラグとレジストリから対象サイト名を決める。
// `enqueue`（site 束縛ジョブ）・`rescue`・`shadow-diff` の 3 コマンドが共有する
// 解決規則（issue #183 の「含むもの」6 で enqueue に導入、issue #533 で
// rescue / shadow-diff にも一般化した）。
//
// 未指定かつレジストリが 1 要素ならその 1 つ、2 要素以上なら `--site` が必須
// （M4-6 の CronJob がサイトごとに投入するため）。指定された名前がレジストリに
// 存在しない場合はエラーにする（typo の早期検出。無音でどの site にも一致しない
// まま成功させない）。
func resolveSiteFlag(cmd *cobra.Command, registry []config.MirakcSite) (string, error) {
	site, err := cmd.Flags().GetString("site")
	if err != nil {
		return "", err
	}

	if site == "" {
		switch len(registry) {
		case 0:
			return "", fmt.Errorf("mirakc registry is empty")
		case 1:
			return registry[0].Site, nil
		default:
			return "", fmt.Errorf("--site is required: mirakcs has %d entries", len(registry))
		}
	}

	for _, s := range registry {
		if s.Site == site {
			return site, nil
		}
	}
	return "", fmt.Errorf("--site: unknown site %q (registry has: %s)", site, strings.Join(registryNames(registry), ", "))
}
