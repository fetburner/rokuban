package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/worker"
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
// **watcher** は mirakc への長期接続を 1 つだけ持つループで、record id /
// schedule が mirakc インスタンス単位のスコープしか持たないため、0 サイト
// （中央プロセスでは動かす対象が無い）でも 2 サイト以上（1 プロセスが N サイトの
// watcher を回す形の書き手がまだいない。不変条件 11）でも起動エラーにする ---
// ちょうど 1 サイトへの束縛だけを許す。
//
// **worker** は今のところ site 単位の仕事（ingest/epg/ruler/reconcile/record_sweep）
// と site 非依存の仕事（encode/thumbnail/delete_reconcile/catalog_export）が
// 同一ロールに同居しており（M4-13/M4-14 で分離される予定。issue #183 はそれを
// 含まない）、worker.Deps.Site / worker.ClientConfig の各 *Site フィールドが
// いずれも単一の文字列であるため、2 サイト以上には束縛できない。
//
// 0 サイト（中央プロセス）は無条件には許さない --- `worker.RequiresSiteBinding`
// が true（`worker.queues` が空、または ingest/epg/ruler/reconciler/watcher の
// いずれかを含む）なら、届く site 単位のジョブが Deps.Site="" と一致せず
// 全滅して再試行し続けるだけの構成になるので起動エラーにする。0 サイトの worker を
// 許すのは `worker.queues` を encode/thumbnail 等の site 非依存キューに絞った
// ときだけ（各 *ClientConfig.*Site フィールドは空文字列のままになり、site 単位の
// 定期ジョブ登録は自然に無効化される。worker.ClientConfig のフィールドコメント参照）。
func validateSiteBinding(roles []string, bound []config.MirakcSite, queues []string) error {
	if slices.Contains(roles, "watcher") && len(bound) != 1 {
		return fmt.Errorf("--sites: watcher role requires exactly one bound site, got %d "+
			"(running N-site watcher loops in a single process is not implemented; issue #183)", len(bound))
	}
	if slices.Contains(roles, "worker") {
		if len(bound) >= 2 {
			return fmt.Errorf("--sites: %d sites bound, but worker role supports at most one bound site "+
				"(worker.Deps.Site and worker.ClientConfig's *Site fields are single strings; issue #183)", len(bound))
		}
		if len(bound) == 0 && worker.RequiresSiteBinding(queues) {
			return fmt.Errorf("--sites: worker role is unbound (central process) but worker.queues %v "+
				"still includes site-bound queues (or is empty, meaning all queues); "+
				"restrict worker.queues to site-independent queues (encode/thumbnail/catalog_export/"+
				"delete_reconcile) or bind to exactly one site with --sites", queues)
		}
	}
	return nil
}

// requireSingleSite は registry がちょうど 1 要素であることを要求する。
//
// rescue / shadow-diff は単一サイト用のまま（issue #183 の「含むもの」7）:
// 両者とも多サイトでの意味論（複数サイトの catalog をどう束ねるか / EPGStation
// も site ごとに分かれるのか）を決める書き手がまだいない（不変条件 11）ので、
// 形を決めずに明示的なエラーで落とす。
func requireSingleSite(registry []config.MirakcSite, cmdName string) (config.MirakcSite, error) {
	switch len(registry) {
	case 0:
		return config.MirakcSite{}, fmt.Errorf("mirakc registry is empty")
	case 1:
		return registry[0], nil
	default:
		return config.MirakcSite{}, fmt.Errorf(
			"%s: multi-site config (mirakcs has %d entries) is not supported yet; issue #183 leaves this undecided",
			cmdName, len(registry))
	}
}

// newBoundBacklogCollector は束縛サイト数がちょうど 1 のときだけ
// metrics.BacklogCollector を作る。
//
// BacklogCollector は「このサイトで未 ingest の record がどれだけ滞留しているか」
// という site 束縛の観測（internal/metrics/backlog.go）。束縛が無い（中央）
// プロセスや 2 サイト以上に束縛されたプロセスでは「このサイト」が定まらないので
// 登録しない --- 登録すると担当していないサイトの系列を出してしまう
// （issue #183 の受け入れ基準）。
//
// **戻り値の型は具体型 `*metrics.BacklogCollector` ではなく
// `prometheus.Collector`（インターフェース）にする。** 具体型のまま nil を返すと、
// 呼び出し側でその値をインターフェース引数（`metrics.NewRegistry` の
// `backlog prometheus.Collector`）に渡した瞬間、型情報付きの非 nil インターフェース
// 値になってしまう（Go の典型的な「具体型 nil を interface に入れると nil でなくなる」
// 罠）。`internal/metrics/metrics.go` の `if backlog != nil` はその非 nil
// インターフェース値を真と判定し、`Register` が nil レシーバの `Describe` を呼んで
// panic する。ここで真の nil インターフェースを返すことで、呼び出し側の
// `!= nil` 判定が正しく機能する。
func newBoundBacklogCollector(pool *pgxpool.Pool, bound []config.MirakcSite) prometheus.Collector {
	if len(bound) != 1 {
		return nil
	}
	return metrics.NewBacklogCollector(pool, bound[0].Site)
}

// resolveEnqueueSite は `enqueue` の `--site` フラグとレジストリから投入先の
// サイト名を決める。
//
// 未指定かつレジストリが 1 要素ならその 1 つ、2 要素以上なら `--site` が必須
// （issue #183 の「含むもの」6。M4-6 の CronJob がサイトごとに投入するため）。
// 指定された名前がレジストリに存在しない場合はエラーにする（typo の早期検出）。
func resolveEnqueueSite(cmd *cobra.Command, registry []config.MirakcSite) (string, error) {
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
