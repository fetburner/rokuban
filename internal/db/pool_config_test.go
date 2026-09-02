package db

import (
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
)

// testDBConfig は接続を試みない設定のビルドだけを検証するための最小 DBConfig。
// pgxpool.ParseConfig は文字列を解釈するだけで実接続はしないため、実在しない
// ホストでも buildPoolConfig の単体テストには使える。
func testDBConfig() config.DBConfig {
	return config.DBConfig{
		Host:     "db.invalid",
		Port:     5432,
		User:     "u",
		Password: "p",
		Database: "d",
		SSLMode:  "disable",
	}
}

func TestBuildPoolConfig_MaxConnsFromRoles(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.DBConfig
		// nil roles ではなく明示的な roles を渡すケースだけ厳密な値を検証する。
		roles []string
		want  int32
	}{
		{name: "api alone", cfg: testDBConfig(), roles: []string{"api"}, want: 10},
		{name: "worker alone", cfg: testDBConfig(), roles: []string{"worker"}, want: 8},
		{name: "watcher alone", cfg: testDBConfig(), roles: []string{"watcher"}, want: 3},
		{name: "notifier alone", cfg: testDBConfig(), roles: []string{"notifier"}, want: 3},
		{name: "streamer alone", cfg: testDBConfig(), roles: []string{"streamer"}, want: 4},
		{
			name:  "all roles (monolith --all)",
			cfg:   testDBConfig(),
			roles: []string{"api", "worker", "watcher", "streamer", "notifier"},
			want:  28, // 10 + 8 + 3 + 4 + 3
		},
		{
			name:  "unknown role only falls back to the minimum (never 0)",
			cfg:   testDBConfig(),
			roles: []string{"totally-unknown-role"},
			want:  minAutoMaxConns,
		},
		{
			// --roles api,api のような重複指定を resolveRoles はそのまま通すため、
			// db 側で重複除去しないと budget を二重に数えてしまう（issue #90 レビュー）。
			name:  "duplicate role names are not double-counted",
			cfg:   testDBConfig(),
			roles: []string{"api", "api"},
			want:  10,
		},
		{
			name:  "duplicate role names across a larger set are not double-counted",
			cfg:   testDBConfig(),
			roles: []string{"api", "worker", "worker", "api", "watcher"},
			want:  21, // 10(api) + 8(worker) + 3(watcher), each counted once
		},
		{
			name: "explicit db.max_conns overrides role-derived sizing",
			cfg: func() config.DBConfig {
				c := testDBConfig()
				c.MaxConns = 99
				return c
			}(),
			roles: []string{"api", "worker", "watcher", "streamer", "notifier"},
			want:  99,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poolCfg, err := buildPoolConfig(tc.cfg, tc.roles, 1)
			if err != nil {
				t.Fatalf("buildPoolConfig: %v", err)
			}
			if poolCfg.MaxConns != tc.want {
				t.Errorf("MaxConns = %d, want %d", poolCfg.MaxConns, tc.want)
			}
		})
	}
}

// TestBuildPoolConfig_MaxConnsFromRoles_MultiSite は issue #532 のレビュー指摘を
// 固定する: 2 サイト以上の束縛では watcher / worker の budget に
// perSiteConnBudget（2 site 目以降 1 site につき watcherPerSiteConns /
// workerPerSiteConns）が上乗せされる。1 site の値（上の
// TestBuildPoolConfig_MaxConnsFromRoles）を変えずに、2 site 目以降だけ増える
// ことを見る。
func TestBuildPoolConfig_MaxConnsFromRoles_MultiSite(t *testing.T) {
	cases := []struct {
		name     string
		roles    []string
		numSites int
		want     int32
	}{
		{name: "watcher, 2 sites: +1 per extra site", roles: []string{"watcher"}, numSites: 2, want: 4},
		{name: "watcher, 3 sites: +1 per extra site", roles: []string{"watcher"}, numSites: 3, want: 5},
		{name: "worker, 2 sites: +2 per extra site", roles: []string{"worker"}, numSites: 2, want: 10},
		{
			name:     "watcher+worker, 2 sites: both budgets get the per-site addition",
			roles:    []string{"watcher", "worker"},
			numSites: 2,
			want:     14,
		},
		{name: "api alone, 2 sites: unaffected (not a site-scoped role)", roles: []string{"api"}, numSites: 2, want: 10},
		{name: "watcher, 1 site: no addition (baseline)", roles: []string{"watcher"}, numSites: 1, want: 3},
		{name: "watcher, 0 sites (unbound): no addition", roles: []string{"watcher"}, numSites: 0, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poolCfg, err := buildPoolConfig(testDBConfig(), tc.roles, tc.numSites)
			if err != nil {
				t.Fatalf("buildPoolConfig: %v", err)
			}
			if poolCfg.MaxConns != tc.want {
				t.Errorf("MaxConns = %d, want %d", poolCfg.MaxConns, tc.want)
			}
		})
	}
}

// TestBuildPoolConfig_NoRoles_UsesPgxDefault は roles が空（単発 CLI コマンド）のとき
// db.max_conns が未指定なら pgxpool 自身の既定値（ParseConfig が埋める値）が
// そのまま使われ、roleConnBudget による上書きが起きないことを確認する。
func TestBuildPoolConfig_NoRoles_UsesPgxDefault(t *testing.T) {
	baseline, err := pgxpool.ParseConfig(testDBConfig().DSN())
	if err != nil {
		t.Fatalf("baseline ParseConfig: %v", err)
	}

	poolCfg, err := buildPoolConfig(testDBConfig(), nil, 0)
	if err != nil {
		t.Fatalf("buildPoolConfig: %v", err)
	}

	if poolCfg.MaxConns != baseline.MaxConns {
		t.Errorf("MaxConns = %d, want pgxpool default %d", poolCfg.MaxConns, baseline.MaxConns)
	}
}

// TestBuildPoolConfig_ExplicitMaxConnsTooSmall は、明示された db.max_conns が
// 生存期間中コネクションを専有し続けるロール（watcher の advisory lock /
// worker・notifier の LISTEN）にとって小さすぎる場合に fail-fast することを確認する
// （issue #90 レビュー指摘）。専有分だけでプールが埋まると、同じプロセスの他の仕事が
// 「二度と解放されないコネクション」を待ち続けて無症状にデッドロックする。
func TestBuildPoolConfig_ExplicitMaxConnsTooSmall(t *testing.T) {
	cases := []struct {
		name     string
		maxConns int
		roles    []string
		wantErr  bool
	}{
		{name: "api alone: 1 is enough (no dedicated connection)", maxConns: 1, roles: []string{"api"}, wantErr: false},
		{name: "watcher alone: 1 is too small (advisory lock would starve other work)", maxConns: 1, roles: []string{"watcher"}, wantErr: true},
		{name: "watcher alone: 2 is enough", maxConns: 2, roles: []string{"watcher"}, wantErr: false},
		{name: "worker alone: 1 is too small (River's LISTEN conn would starve job claims)", maxConns: 1, roles: []string{"worker"}, wantErr: true},
		{name: "worker alone: 2 is enough", maxConns: 2, roles: []string{"worker"}, wantErr: false},
		{name: "notifier alone: 1 is too small (LISTEN conn would starve other work)", maxConns: 1, roles: []string{"notifier"}, wantErr: true},
		{name: "notifier alone: 2 is enough", maxConns: 2, roles: []string{"notifier"}, wantErr: false},
		{
			name:     "watcher+notifier: 2 dedicated conns need at least 3",
			maxConns: 2,
			roles:    []string{"watcher", "notifier"},
			wantErr:  true,
		},
		{
			name:     "watcher+notifier: 3 is enough",
			maxConns: 3,
			roles:    []string{"watcher", "notifier"},
			wantErr:  false,
		},
		{
			name:     "--all: 3 dedicated conns (watcher+worker+notifier) need at least 4",
			maxConns: 3,
			roles:    []string{"api", "worker", "watcher", "streamer", "notifier"},
			wantErr:  true,
		},
		{
			name:     "--all: 4 is enough",
			maxConns: 4,
			roles:    []string{"api", "worker", "watcher", "streamer", "notifier"},
			wantErr:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testDBConfig()
			cfg.MaxConns = tc.maxConns
			_, err := buildPoolConfig(cfg, tc.roles, 1)
			if tc.wantErr && err == nil {
				t.Errorf("buildPoolConfig(max_conns=%d, roles=%v): expected error, got nil", tc.maxConns, tc.roles)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("buildPoolConfig(max_conns=%d, roles=%v): unexpected error: %v", tc.maxConns, tc.roles, err)
			}
		})
	}
}

// TestBuildPoolConfig_ExplicitMaxConnsTooSmall_MultiSite は issue #532 のレビュー
// 指摘そのものを固定する: `--roles watcher --sites tokyo,takamatsu --db.max_conns 2`
// は以前の（site 数を見ない）fail-fast を通り抜けてしまっていた --- 2 site の
// watcher は advisory lock 用に 2 本を専有するので、2 本しか無いプールでは
// 他の仕事（record 処理クエリ等）が二度と進めない無症状デッドロックになる。
func TestBuildPoolConfig_ExplicitMaxConnsTooSmall_MultiSite(t *testing.T) {
	cases := []struct {
		name     string
		maxConns int
		roles    []string
		numSites int
		wantErr  bool
	}{
		{
			name:     "watcher, 2 sites, max_conns=2 is too small (2 sites pin both connections, nothing left for queries)",
			maxConns: 2,
			roles:    []string{"watcher"},
			numSites: 2,
			wantErr:  true,
		},
		{
			name:     "watcher, 2 sites, max_conns=3 is enough",
			maxConns: 3,
			roles:    []string{"watcher"},
			numSites: 2,
			wantErr:  false,
		},
		{
			name:     "watcher, 3 sites, max_conns=3 is too small (3 sites pin all 3, nothing left)",
			maxConns: 3,
			roles:    []string{"watcher"},
			numSites: 3,
			wantErr:  true,
		},
		{
			name:     "watcher, 3 sites, max_conns=4 is enough",
			maxConns: 4,
			roles:    []string{"watcher"},
			numSites: 3,
			wantErr:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testDBConfig()
			cfg.MaxConns = tc.maxConns
			_, err := buildPoolConfig(cfg, tc.roles, tc.numSites)
			if tc.wantErr && err == nil {
				t.Errorf("buildPoolConfig(max_conns=%d, roles=%v, numSites=%d): expected error, got nil",
					tc.maxConns, tc.roles, tc.numSites)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("buildPoolConfig(max_conns=%d, roles=%v, numSites=%d): unexpected error: %v",
					tc.maxConns, tc.roles, tc.numSites, err)
			}
		})
	}
}

func TestBuildPoolConfig_APIStatementTimeout(t *testing.T) {
	t.Run("api role: unset uses the built-in default", func(t *testing.T) {
		poolCfg, err := buildPoolConfig(testDBConfig(), []string{"api"}, 1)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		got := poolCfg.ConnConfig.RuntimeParams["statement_timeout"]
		want := strconv.FormatInt(defaultAPIStatementTimeout.Milliseconds(), 10)
		if got != want {
			t.Errorf("statement_timeout RuntimeParam = %q, want %q", got, want)
		}
	})

	t.Run("api role: explicit value is honored", func(t *testing.T) {
		cfg := testDBConfig()
		cfg.APIStatementTimeout = 5 * time.Second
		poolCfg, err := buildPoolConfig(cfg, []string{"api"}, 1)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		got := poolCfg.ConnConfig.RuntimeParams["statement_timeout"]
		if got != "5000" {
			t.Errorf("statement_timeout RuntimeParam = %q, want %q", got, "5000")
		}
	})

	t.Run("no api role: statement_timeout is not set", func(t *testing.T) {
		poolCfg, err := buildPoolConfig(testDBConfig(), []string{"worker"}, 1)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if _, ok := poolCfg.ConnConfig.RuntimeParams["statement_timeout"]; ok {
			t.Errorf("statement_timeout RuntimeParam set for a process without the api role: %q",
				poolCfg.ConnConfig.RuntimeParams["statement_timeout"])
		}
	})
}

func TestBuildPoolConfig_PoolerCompat(t *testing.T) {
	t.Run("api role: allowed, disables prepared statement caching", func(t *testing.T) {
		cfg := testDBConfig()
		cfg.PoolerCompat = true
		poolCfg, err := buildPoolConfig(cfg, []string{"api"}, 1)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if poolCfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
			t.Errorf("DefaultQueryExecMode = %v, want QueryExecModeExec", poolCfg.ConnConfig.DefaultQueryExecMode)
		}
	})

	t.Run("streamer role: allowed", func(t *testing.T) {
		cfg := testDBConfig()
		cfg.PoolerCompat = true
		if _, err := buildPoolConfig(cfg, []string{"streamer"}, 1); err != nil {
			t.Errorf("buildPoolConfig: unexpected error for streamer + pooler_compat: %v", err)
		}
	})

	for _, role := range []string{"worker", "watcher", "notifier"} {
		t.Run(role+" role: fail-fast", func(t *testing.T) {
			cfg := testDBConfig()
			cfg.PoolerCompat = true
			_, err := buildPoolConfig(cfg, []string{role}, 1)
			if err == nil {
				t.Fatalf("expected error for pooler_compat + %s, got nil", role)
			}
		})
	}

	t.Run("mixed roles: any incompatible role fails even alongside api", func(t *testing.T) {
		cfg := testDBConfig()
		cfg.PoolerCompat = true
		_, err := buildPoolConfig(cfg, []string{"api", "worker"}, 1)
		if err == nil {
			t.Fatal("expected error for pooler_compat + api,worker, got nil")
		}
	})

	t.Run("disabled: DefaultQueryExecMode is untouched", func(t *testing.T) {
		poolCfg, err := buildPoolConfig(testDBConfig(), []string{"api"}, 1)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if poolCfg.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeExec {
			t.Error("DefaultQueryExecMode should not be QueryExecModeExec when pooler_compat is false")
		}
	})
}
