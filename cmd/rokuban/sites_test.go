package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/testutil"
)

func newSitesTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringSlice(siteFlagName, nil, "")
	return cmd
}

var (
	tokyo     = config.MirakcSite{Site: "tokyo", URL: "http://mirakc-tokyo:40772"}
	takamatsu = config.MirakcSite{Site: "takamatsu", URL: "http://mirakc-takamatsu:40772"}
)

func TestResolveSiteBinding_UnspecifiedSingleEntryRegistry(t *testing.T) {
	cmd := newSitesTestCmd(t)
	bound, err := resolveSiteBinding(cmd, []config.MirakcSite{tokyo})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bound) != 1 || bound[0] != tokyo {
		t.Errorf("bound = %+v, want [tokyo]", bound)
	}
}

// レジストリが 2 要素以上あるのに --sites を省略したら起動エラーになること
// （暗黙に「全部」にしない。issue #183 の受け入れ基準）。
func TestResolveSiteBinding_UnspecifiedMultiEntryRegistry_IsError(t *testing.T) {
	cmd := newSitesTestCmd(t)
	_, err := resolveSiteBinding(cmd, []config.MirakcSite{tokyo, takamatsu})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// --sites=（明示的な空）は束縛なし（中央プロセス）であり、--sites 省略とは
// 区別されること（issue #183 の「罠」: pflag の StringSlice はどちらも長さ 0 の
// スライスになるので Changed() で判定する必要がある）。
func TestResolveSiteBinding_ExplicitEmpty_MeansUnbound(t *testing.T) {
	cmd := newSitesTestCmd(t)
	if err := cmd.Flags().Set(siteFlagName, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	bound, err := resolveSiteBinding(cmd, []config.MirakcSite{tokyo, takamatsu})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bound) != 0 {
		t.Errorf("bound = %+v, want empty (central process)", bound)
	}
}

func TestResolveSiteBinding_ExplicitSite_BindsToThatSite(t *testing.T) {
	cmd := newSitesTestCmd(t)
	if err := cmd.Flags().Set(siteFlagName, "tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	bound, err := resolveSiteBinding(cmd, []config.MirakcSite{tokyo, takamatsu})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bound) != 1 || bound[0] != tokyo {
		t.Errorf("bound = %+v, want [tokyo]", bound)
	}
}

// --sites tokyo,tokyo のような重複指定は 1 サイトに畳む。畳まずに 2 要素として
// 束縛すると、watcher/worker ロールが「2 サイト束縛」という紛らわしいエラーで
// 落ちてしまう（issue #183 のレビュー指摘）。
func TestResolveSiteBinding_DuplicateNames_AreFolded(t *testing.T) {
	cmd := newSitesTestCmd(t)
	if err := cmd.Flags().Set(siteFlagName, "tokyo,tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	bound, err := resolveSiteBinding(cmd, []config.MirakcSite{tokyo, takamatsu})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bound) != 1 || bound[0] != tokyo {
		t.Errorf("bound = %+v, want [tokyo] (folded)", bound)
	}
}

func TestResolveSiteBinding_UnknownSite_IsError(t *testing.T) {
	cmd := newSitesTestCmd(t)
	if err := cmd.Flags().Set(siteFlagName, "osaka"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveSiteBinding(cmd, []config.MirakcSite{tokyo, takamatsu})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "osaka") {
		t.Errorf("error = %v, want mention of osaka", err)
	}
}

func TestValidateSiteBinding(t *testing.T) {
	tests := []struct {
		name    string
		roles   []string
		bound   []config.MirakcSite
		queues  []string
		wantErr bool
	}{
		{"watcher with exactly one site is fine", []string{"watcher"}, []config.MirakcSite{tokyo}, nil, false},
		// issue #532: 1 プロセスが N site の watcher goroutine を回せるようになった
		// ので、watcher ロールの束縛サイト数に対する検査そのものが無い（0 サイトは
		// 「何も watch しない」という無害な構成、2 サイト以上は site ごとに
		// goroutine + advisory lock を持つので安全）。
		{"watcher with zero sites is fine (watches nothing)", []string{"watcher"}, nil, nil, false},
		{"watcher with two sites is fine (one goroutine per site)", []string{"watcher"}, []config.MirakcSite{tokyo, takamatsu}, nil, false},
		{
			"worker with zero sites and default (all) queues is an error " +
				"(ingest/epg/ruler/reconciler/watcher would get an unresolvable empty site)",
			[]string{"worker"}, nil, nil, true,
		},
		{
			"worker with zero sites and queues including ingest is an error",
			[]string{"worker"}, nil, []string{"ingest", "encode"}, true,
		},
		{
			"worker with zero sites and queues restricted to site-independent ones is fine",
			[]string{"worker"}, nil, []string{"encode", "thumbnail"}, false,
		},
		{"worker with one site and default queues is fine", []string{"worker"}, []config.MirakcSite{tokyo}, nil, false},
		// issue #532: worker.Deps.MirakcClients / worker.ClientConfig.BoundSites が
		// site → 値の map / 集合になったので、2 サイト以上への束縛も起動できる。
		{"worker with two sites is fine (N-site binding)", []string{"worker"}, []config.MirakcSite{tokyo, takamatsu}, nil, false},
		{
			"worker with zero sites and queues restricted to ruler (site-independent, issue #185) is fine",
			[]string{"worker"}, nil, []string{"ruler"}, false,
		},
		{
			"worker with zero sites and queues restricted to cleanup (issue #185, new queue) is fine",
			[]string{"worker"}, nil, []string{"cleanup"}, false,
		},
		{
			"worker with zero sites and queues including reconciler is an error",
			[]string{"worker"}, nil, []string{"reconciler", "ruler"}, true,
		},
		{
			"api alone with two sites is fine (api doesn't need a single site)",
			[]string{"api"}, []config.MirakcSite{tokyo, takamatsu}, nil, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSiteBinding(tt.roles, tt.bound, tt.queues)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRequireSingleSite(t *testing.T) {
	t.Run("one entry resolves", func(t *testing.T) {
		s, err := requireSingleSite([]config.MirakcSite{tokyo}, "rescue")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s != tokyo {
			t.Errorf("s = %+v, want tokyo", s)
		}
	})

	t.Run("multi-site registry is an error", func(t *testing.T) {
		_, err := requireSingleSite([]config.MirakcSite{tokyo, takamatsu}, "rescue")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "rescue") {
			t.Errorf("error = %v, want mention of the command name", err)
		}
	})
}

// --sites=（明示的な空、束縛なし）で起動したプロセスは BacklogCollector を
// 1 つも登録しない --- 担当していないサイトの系列を /metrics に出さないため
// （issue #183 の受け入れ基準）。N サイト束縛では束縛サイトの数だけ登録する
// （issue #532: 旧「2 サイト以上は登録しない」判断は誤りだった。系列は
// site ラベルでそれぞれ区別できるので曖昧にならない）。
func TestNewBoundBacklogCollector(t *testing.T) {
	pool := testutil.SetupDB(t)

	sitesOf := func(t *testing.T, reg *prometheus.Registry) []string {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		var sites []string
		for _, f := range families {
			if f.GetName() != "rokuban_uningested_records" {
				continue
			}
			for _, m := range f.Metric {
				for _, l := range m.Label {
					if l.GetName() == "site" {
						sites = append(sites, l.GetValue())
					}
				}
			}
		}
		return sites
	}

	t.Run("unbound (central) process registers nothing", func(t *testing.T) {
		cs := newBoundBacklogCollectors(pool, nil)
		if len(cs) != 0 {
			t.Fatalf("collectors = %v, want none", cs)
		}
	})

	t.Run("two bound sites registers one collector per site", func(t *testing.T) {
		cs := newBoundBacklogCollectors(pool, []config.MirakcSite{tokyo, takamatsu})
		if len(cs) != 2 {
			t.Fatalf("collectors = %d, want 2", len(cs))
		}
		reg := prometheus.NewRegistry()
		for _, c := range cs {
			if err := reg.Register(c); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
		sites := sitesOf(t, reg)
		if !slices.Contains(sites, "tokyo") || !slices.Contains(sites, "takamatsu") {
			t.Errorf("site labels = %v, want both tokyo and takamatsu", sites)
		}
	})

	t.Run("exactly one bound site registers a collector reporting under that site's series", func(t *testing.T) {
		cs := newBoundBacklogCollectors(pool, []config.MirakcSite{tokyo})
		if len(cs) != 1 {
			t.Fatalf("collectors = %d, want 1", len(cs))
		}
		reg := prometheus.NewRegistry()
		if err := reg.Register(cs[0]); err != nil {
			t.Fatalf("Register: %v", err)
		}
		sites := sitesOf(t, reg)
		if len(sites) == 0 {
			t.Error("rokuban_uningested_records not found in gathered metrics")
		}
		for _, s := range sites {
			if s != "tokyo" {
				t.Errorf("site label = %q, want tokyo", s)
			}
		}
	})
}

// TestNewBoundBacklogCollector_ThroughNewRegistry は server.go が実際に組む配線
// （newBoundBacklogCollectors の戻り値を直接 metrics.NewRegistry(backlog...) に
// 渡す）をエンドツーエンドで再現する。
//
// これは「具体型 nil を interface 引数に渡すと非 nil interface になる」という Go の
// 罠を回帰させないための独立したテストである。newBoundBacklogCollectors が
// 要素に具体型 nil を混ぜる実装に戻ると、ここで `metrics.NewRegistry` に渡した
// 瞬間に型情報付きの非 nil interface 値になり、`prometheus.Registry.Register` が
// nil レシーバーの `Describe` を呼び panic する（`--sites=` で起動した実バイナリで
// 踏んだ実例。issue #183 のレビュー指摘）。前段の TestNewBoundBacklogCollector は
// 戻り値をローカル変数（すでに prometheus.Collector 型）で見るだけなので、関数の
// 戻り値の型そのものが具体型に戻る回帰を捕まえられない。ここは必ず
// metrics.NewRegistry を経由させることで、型の選択そのものを検証する。
func TestNewBoundBacklogCollector_ThroughNewRegistry(t *testing.T) {
	pool := testutil.SetupDB(t)

	sitesWithBacklogSeries := func(t *testing.T, reg *prometheus.Registry) []string {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		var sites []string
		for _, f := range families {
			if f.GetName() != "rokuban_uningested_records" {
				continue
			}
			for _, m := range f.Metric {
				for _, l := range m.Label {
					if l.GetName() == "site" {
						sites = append(sites, l.GetValue())
					}
				}
			}
		}
		return sites
	}

	t.Run("unbound process: NewRegistry+Gather does not panic and has no backlog series", func(t *testing.T) {
		backlog := newBoundBacklogCollectors(pool, nil)
		reg := metrics.NewRegistry(backlog...) // 具体型 nil が漏れていればここで panic する
		if sites := sitesWithBacklogSeries(t, reg); len(sites) != 0 {
			t.Errorf("unbound process should not expose rokuban_uningested_records, got sites %v", sites)
		}
	})

	t.Run("two bound sites: NewRegistry+Gather exposes both sites' backlog series", func(t *testing.T) {
		backlog := newBoundBacklogCollectors(pool, []config.MirakcSite{tokyo, takamatsu})
		reg := metrics.NewRegistry(backlog...) // 具体型 nil が漏れていればここで panic する
		sites := sitesWithBacklogSeries(t, reg)
		if !slices.Contains(sites, "tokyo") || !slices.Contains(sites, "takamatsu") {
			t.Errorf("2-site binding site labels = %v, want both tokyo and takamatsu", sites)
		}
	})

	t.Run("exactly one bound site: NewRegistry+Gather exposes the backlog series", func(t *testing.T) {
		backlog := newBoundBacklogCollectors(pool, []config.MirakcSite{tokyo})
		reg := metrics.NewRegistry(backlog...)
		if sites := sitesWithBacklogSeries(t, reg); len(sites) == 0 {
			t.Error("single-site binding should expose rokuban_uningested_records")
		}
	})
}

// newSiteFlagTestCmd は resolveSiteFlag のテストが使う、`--site` フラグだけを
// 持つ最小の cobra.Command を作る（enqueue / rescue / shadow-diff の 3 コマンドが
// 共有する解決規則なので、コマンド名には依存しない）。
func newSiteFlagTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("site", "", "")
	return cmd
}

// TestResolveSiteFlag は enqueue（site 束縛ジョブ）・rescue・shadow-diff が
// 共有する `--site` 解決規則の単体テスト（issue #183 の「含むもの」6 で導入、
// issue #533 で rescue / shadow-diff にも一般化した）。
func TestResolveSiteFlag(t *testing.T) {
	t.Run("unspecified with single-entry registry", func(t *testing.T) {
		cmd := newSiteFlagTestCmd(t)
		site, err := resolveSiteFlag(cmd, []config.MirakcSite{tokyo})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if site != "tokyo" {
			t.Errorf("site = %q, want tokyo", site)
		}
	})

	t.Run("unspecified with multi-entry registry is an error", func(t *testing.T) {
		cmd := newSiteFlagTestCmd(t)
		_, err := resolveSiteFlag(cmd, []config.MirakcSite{tokyo, takamatsu})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("explicit site resolves", func(t *testing.T) {
		cmd := newSiteFlagTestCmd(t)
		if err := cmd.Flags().Set("site", "takamatsu"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		site, err := resolveSiteFlag(cmd, []config.MirakcSite{tokyo, takamatsu})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if site != "takamatsu" {
			t.Errorf("site = %q, want takamatsu", site)
		}
	})

	// --site の値はレジストリ照合する（issue #533 の「罠」: タイポを
	// 「どの site にも一致しない」として無音で成功させない）。この変異
	// （照合を外して常に成功させる）はここで落ちる。
	t.Run("explicit unknown site is an error", func(t *testing.T) {
		cmd := newSiteFlagTestCmd(t)
		if err := cmd.Flags().Set("site", "osaka"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		_, err := resolveSiteFlag(cmd, []config.MirakcSite{tokyo, takamatsu})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
