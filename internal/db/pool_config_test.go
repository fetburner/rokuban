package db

import (
	"strconv"
	"testing"
	"time"

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

// defaultLockSlots は本番の既定構成（ingest 3 + cm_detect 1、1 site。
// internal/worker.LockSlots）を模した値。worker を含むケースの予算はこの値から
// 導出されるので、テストは表のリテラルではなくこれを渡す。
const defaultLockSlots = 4

func TestBuildPoolConfig_MaxConnsFromRoles(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.DBConfig
		// nil roles ではなく明示的な roles を渡すケースだけ厳密な値を検証する。
		roles     []string
		lockSlots int
		want      int32
	}{
		{name: "api alone", cfg: testDBConfig(), roles: []string{"api"}, want: 10},
		{
			// worker の予算は表の 8 ではなく lockSlots から導出される
			// （1 + lock 4 + slack 3 = 8）。
			name: "worker alone", cfg: testDBConfig(), roles: []string{"worker"},
			lockSlots: defaultLockSlots, want: 8,
		},
		{
			// 式が床を下回るときは床が効く（1 + 0 + 3 = 4 < 8）。
			name: "worker alone with no lock slots still gets the floor",
			cfg:  testDBConfig(), roles: []string{"worker"}, lockSlots: 0, want: 8,
		},
		{name: "watcher alone", cfg: testDBConfig(), roles: []string{"watcher"}, want: 3},
		{name: "notifier alone", cfg: testDBConfig(), roles: []string{"notifier"}, want: 3},
		{name: "streamer alone", cfg: testDBConfig(), roles: []string{"streamer"}, want: 4},
		{
			name:  "all roles (monolith --all)",
			cfg:   testDBConfig(),
			roles: []string{"api", "worker", "watcher", "streamer", "notifier"},
			// 10 + 8(worker: 1 + 4 + 3) + 3 + 4 + 3
			lockSlots: defaultLockSlots,
			want:      28,
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
			name:      "duplicate role names across a larger set are not double-counted",
			cfg:       testDBConfig(),
			roles:     []string{"api", "worker", "worker", "api", "watcher"},
			lockSlots: defaultLockSlots,
			want:      21, // 10(api) + 8(worker) + 3(watcher), each counted once
		},
		{
			name: "explicit db.max_conns overrides role-derived sizing",
			cfg: func() config.DBConfig {
				c := testDBConfig()
				c.MaxConns = 99
				return c
			}(),
			roles:     []string{"api", "worker", "watcher", "streamer", "notifier"},
			lockSlots: defaultLockSlots,
			want:      99,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poolCfg, err := buildPoolConfig(tc.cfg, tc.roles, 1, tc.lockSlots)
			if err != nil {
				t.Fatalf("buildPoolConfig: %v", err)
			}
			if poolCfg.MaxConns != tc.want {
				t.Errorf("MaxConns = %d, want %d", poolCfg.MaxConns, tc.want)
			}
		})
	}
}

// TestBuildPoolConfig_WorkerBudgetFollowsLockSlots は worker の予算が
// 「同時に走りうる job advisory lock 保持ジョブの本数」に追随することと、床 8 を
// 下回らないことを固定する。**床が効くのは式が 8 以下のときだけ**である。
func TestBuildPoolConfig_WorkerBudgetFollowsLockSlots(t *testing.T) {
	cases := []struct {
		lockSlots int
		want      int32
	}{
		{lockSlots: 0, want: 8},   // 式 4 → 床
		{lockSlots: 4, want: 8},   // 式 8 → 床と一致
		{lockSlots: 5, want: 9},   // 式 9 → 床を超える（既定構成）
		{lockSlots: 8, want: 12},  // 式 12
		{lockSlots: 20, want: 24}, // 式 24
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.lockSlots), func(t *testing.T) {
			poolCfg, err := buildPoolConfig(testDBConfig(), []string{"worker"}, 1, tc.lockSlots)
			if err != nil {
				t.Fatalf("buildPoolConfig: %v", err)
			}
			if poolCfg.MaxConns != tc.want {
				t.Errorf("lockSlots=%d: MaxConns = %d, want %d", tc.lockSlots, poolCfg.MaxConns, tc.want)
			}
		})
	}
}

// TestBuildPoolConfig_MaxConnsFromRoles_MultiSite は site 数が予算に効くロールと
// 効かないロールを分けて固定する（issue #532 のレビュー指摘）。
//
// watcher は束縛サイトごとに advisory lock 用コネクションを 1 本専有するので
// perSiteConnBudget が上乗せされる。**worker は上乗せされない** --- 2 site 目の
// ingest job lock は lockSlots として呼び出し元から渡ってくるので、ここで site
// 数から二重に足すと過大になる。
func TestBuildPoolConfig_MaxConnsFromRoles_MultiSite(t *testing.T) {
	cases := []struct {
		name      string
		roles     []string
		numSites  int
		lockSlots int
		want      int32
	}{
		{name: "watcher, 2 sites: +1 per extra site", roles: []string{"watcher"}, numSites: 2, want: 4},
		{name: "watcher, 3 sites: +1 per extra site", roles: []string{"watcher"}, numSites: 3, want: 5},
		{
			name:      "worker, 2 sites: the budget term does not grow with site count",
			roles:     []string{"worker"},
			numSites:  2,
			lockSlots: defaultLockSlots,
			want:      8,
		},
		{
			name:      "worker, 2 sites: the extra per-site ingest locks arrive as lockSlots",
			roles:     []string{"worker"},
			numSites:  2,
			lockSlots: 7, // ingest 3 x 2 sites + cm_detect 1
			want:      11,
		},
		{
			name:      "watcher+worker, 2 sites: only watcher gets the per-site addition",
			roles:     []string{"watcher", "worker"},
			numSites:  2,
			lockSlots: defaultLockSlots,
			want:      12, // 3+1(watcher) + 8(worker)
		},
		{name: "api alone, 2 sites: unaffected (not a site-scoped role)", roles: []string{"api"}, numSites: 2, want: 10},
		{name: "watcher, 1 site: no addition (baseline)", roles: []string{"watcher"}, numSites: 1, want: 3},
		{name: "watcher, 0 sites (unbound): no addition", roles: []string{"watcher"}, numSites: 0, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poolCfg, err := buildPoolConfig(testDBConfig(), tc.roles, tc.numSites, tc.lockSlots)
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

	poolCfg, err := buildPoolConfig(testDBConfig(), nil, 0, 0)
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
		name      string
		maxConns  int
		roles     []string
		lockSlots int
		wantErr   bool
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
			name:      "--all: 3 dedicated conns + 4 job locks need at least 8",
			maxConns:  7,
			roles:     []string{"api", "worker", "watcher", "streamer", "notifier"},
			lockSlots: defaultLockSlots,
			wantErr:   true,
		},
		{
			name:      "--all: 8 is enough",
			maxConns:  8,
			roles:     []string{"api", "worker", "watcher", "streamer", "notifier"},
			lockSlots: defaultLockSlots,
			wantErr:   false,
		},
		{
			// 予算の床（8）は下限には効かない --- 下限は「専有分 + 余地 1」で、
			// 床を混ぜると「今デッドロックしうる構成」以外まで弾いてしまう。
			name:      "worker alone: the budget floor does not raise the fail-fast floor",
			maxConns:  7,
			roles:     []string{"worker"},
			lockSlots: defaultLockSlots,
			wantErr:   false,
		},
		{
			name:      "worker alone: 4 job locks need 1(LISTEN) + 4 + 1 = 6",
			maxConns:  5,
			roles:     []string{"worker"},
			lockSlots: defaultLockSlots,
			wantErr:   true,
		},
		{
			name:      "worker alone: lockSlots growth raises the fail-fast floor too",
			maxConns:  11,
			roles:     []string{"worker"},
			lockSlots: 10,
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testDBConfig()
			cfg.MaxConns = tc.maxConns
			_, err := buildPoolConfig(cfg, tc.roles, 1, tc.lockSlots)
			if tc.wantErr && err == nil {
				t.Errorf("buildPoolConfig(max_conns=%d, roles=%v, lockSlots=%d): expected error, got nil",
					tc.maxConns, tc.roles, tc.lockSlots)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("buildPoolConfig(max_conns=%d, roles=%v, lockSlots=%d): unexpected error: %v",
					tc.maxConns, tc.roles, tc.lockSlots, err)
			}
		})
	}
}

// TestMinRequiredConns_IncludesLockSlots は下限の数え上げを固定する。
//
// **予算の床（8）は下限には混ぜない。** 下限が数えるのは「解放が別の接続取得に
// 依存する専有」だけで、床を混ぜると「まだデッドロックしない構成」まで
// 起動時に弾くことになる。
func TestMinRequiredConns_IncludesLockSlots(t *testing.T) {
	cases := []struct {
		name      string
		roles     []string
		numSites  int
		lockSlots int
		want      int32
	}{
		{name: "api alone: only the room for other work", roles: []string{"api"}, want: 1},
		{name: "worker alone: LISTEN + the room", roles: []string{"worker"}, want: 2},
		{name: "worker alone: job locks add one each", roles: []string{"worker"}, lockSlots: 4, want: 6},
		{name: "worker+watcher+notifier: three dedicated conns each add the room", roles: []string{"worker", "watcher", "notifier"}, lockSlots: 4, want: 8},
		{name: "watcher, 2 sites: one advisory lock per site", roles: []string{"watcher"}, numSites: 2, want: 3},
		{name: "watcher, 1 site", roles: []string{"watcher"}, numSites: 1, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := minRequiredConns(tc.roles, tc.numSites, tc.lockSlots); got != tc.want {
				t.Errorf("minRequiredConns(%v, %d, %d) = %d, want %d",
					tc.roles, tc.numSites, tc.lockSlots, got, tc.want)
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
			_, err := buildPoolConfig(cfg, tc.roles, tc.numSites, 0)
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
		poolCfg, err := buildPoolConfig(testDBConfig(), []string{"api"}, 1, 0)
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
		poolCfg, err := buildPoolConfig(cfg, []string{"api"}, 1, 0)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		got := poolCfg.ConnConfig.RuntimeParams["statement_timeout"]
		if got != "5000" {
			t.Errorf("statement_timeout RuntimeParam = %q, want %q", got, "5000")
		}
	})

	t.Run("no api role: statement_timeout is not set", func(t *testing.T) {
		poolCfg, err := buildPoolConfig(testDBConfig(), []string{"worker"}, 1, 0)
		if err != nil {
			t.Fatalf("buildPoolConfig: %v", err)
		}
		if _, ok := poolCfg.ConnConfig.RuntimeParams["statement_timeout"]; ok {
			t.Errorf("statement_timeout RuntimeParam set for a process without the api role: %q",
				poolCfg.ConnConfig.RuntimeParams["statement_timeout"])
		}
	})
}
