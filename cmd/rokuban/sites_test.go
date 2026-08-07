package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
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
		wantErr bool
	}{
		{"watcher with exactly one site is fine", []string{"watcher"}, []config.MirakcSite{tokyo}, false},
		{"watcher with zero sites is an error", []string{"watcher"}, nil, true},
		{"watcher with two sites is an error", []string{"watcher"}, []config.MirakcSite{tokyo, takamatsu}, true},
		{"worker with zero sites is fine (central, site非依存 queues)", []string{"worker"}, nil, false},
		{"worker with one site is fine", []string{"worker"}, []config.MirakcSite{tokyo}, false},
		{"worker with two sites is an error", []string{"worker"}, []config.MirakcSite{tokyo, takamatsu}, true},
		{"api alone with two sites is fine (api doesn't need a single site)", []string{"api"}, []config.MirakcSite{tokyo, takamatsu}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSiteBinding(tt.roles, tt.bound)
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

// --sites=（明示的な空、束縛なし）で起動したプロセスは BacklogCollector を登録
// しない --- 担当していないサイトの系列を /metrics に出さないため
// （issue #183 の受け入れ基準）。同じ理由で 2 サイト以上の束縛でも登録しない
// （どちらの site の系列として出すべきか定まらない）。
func TestNewBoundBacklogCollector(t *testing.T) {
	pool := testutil.SetupDB(t)

	t.Run("unbound (central) process registers nothing", func(t *testing.T) {
		c := newBoundBacklogCollector(pool, nil)
		if c != nil {
			t.Fatalf("collector = %v, want nil", c)
		}
	})

	t.Run("two bound sites registers nothing (ambiguous which site)", func(t *testing.T) {
		c := newBoundBacklogCollector(pool, []config.MirakcSite{tokyo, takamatsu})
		if c != nil {
			t.Fatalf("collector = %v, want nil", c)
		}
	})

	t.Run("exactly one bound site registers a collector reporting under that site's series", func(t *testing.T) {
		c := newBoundBacklogCollector(pool, []config.MirakcSite{tokyo})
		if c == nil {
			t.Fatal("collector = nil, want non-nil")
		}
		reg := prometheus.NewRegistry()
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register: %v", err)
		}
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		found := false
		for _, f := range families {
			if f.GetName() != "rokuban_uningested_records" {
				continue
			}
			found = true
			for _, m := range f.Metric {
				for _, l := range m.Label {
					if l.GetName() == "site" && l.GetValue() != "tokyo" {
						t.Errorf("site label = %q, want tokyo", l.GetValue())
					}
				}
			}
		}
		if !found {
			t.Error("rokuban_uningested_records not found in gathered metrics")
		}
	})
}

func newEnqueueSiteTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("site", "", "")
	return cmd
}

func TestResolveEnqueueSite(t *testing.T) {
	t.Run("unspecified with single-entry registry", func(t *testing.T) {
		cmd := newEnqueueSiteTestCmd(t)
		site, err := resolveEnqueueSite(cmd, []config.MirakcSite{tokyo})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if site != "tokyo" {
			t.Errorf("site = %q, want tokyo", site)
		}
	})

	t.Run("unspecified with multi-entry registry is an error", func(t *testing.T) {
		cmd := newEnqueueSiteTestCmd(t)
		_, err := resolveEnqueueSite(cmd, []config.MirakcSite{tokyo, takamatsu})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("explicit site resolves", func(t *testing.T) {
		cmd := newEnqueueSiteTestCmd(t)
		if err := cmd.Flags().Set("site", "takamatsu"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		site, err := resolveEnqueueSite(cmd, []config.MirakcSite{tokyo, takamatsu})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if site != "takamatsu" {
			t.Errorf("site = %q, want takamatsu", site)
		}
	})

	t.Run("explicit unknown site is an error", func(t *testing.T) {
		cmd := newEnqueueSiteTestCmd(t)
		if err := cmd.Flags().Set("site", "osaka"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		_, err := resolveEnqueueSite(cmd, []config.MirakcSite{tokyo, takamatsu})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
