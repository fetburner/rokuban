package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/mysqlstore"
)

const dialectBaseConfig = `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: home
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`

// asynqDialectBaseConfig は、方言を書かない設定。engine は asynq にして、既定の方言の値だけを検査できるようにする。
var asynqDialectBaseConfig = dialectBaseConfig + `
worker:
  engine: asynq
  redis_url: redis://localhost:6379/7
`

// mysqlDialectConfig は db.dialect: mysql と asynq の設定（MySQL 構成が起動を許される唯一の組み合わせ）。
var mysqlDialectConfig = strings.Replace(dialectBaseConfig, "db:\n", "db:\n  dialect: mysql\n", 1) + `
worker:
  engine: asynq
  redis_url: redis://localhost:6379/7
`

// TestDefaultDialectIsPostgres は、db.dialect を書かない設定（asynq と組む）が PostgreSQL として読まれ、
// MySQL 専用の掃除ジョブを定期投入しないことを確かめる。config の既定を mysql に変えると、この値の検査が落ちる。
func TestDefaultDialectIsPostgres(t *testing.T) {
	cfg, err := config.Load(writeServerTestConfig(t, asynqDialectBaseConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Dialect != config.DBDialectPostgres {
		t.Fatalf("db.dialect = %q for a config without the key, want %q", cfg.DB.Dialect, config.DBDialectPostgres)
	}
	if hasNotifyPruneSpec(cfg, pgContract()) {
		t.Fatal("PostgreSQL worker specs must not schedule notify_prune")
	}
}

// TestMySQLDialectIsSelected は、db.dialect: mysql の設定が MySQL として読まれることを確かめる。
func TestMySQLDialectIsSelected(t *testing.T) {
	cfg, err := config.Load(writeServerTestConfig(t, mysqlDialectConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Dialect != config.DBDialectMySQL {
		t.Fatalf("db.dialect = %q, want %q", cfg.DB.Dialect, config.DBDialectMySQL)
	}
}

// TestNotifyPruneIntervalWithinRetention は、MySQL の掃除ジョブが保持期間以下の間隔で定期投入され、
// PostgreSQL の定期ジョブには現れないことを確かめる。
func TestNotifyPruneIntervalWithinRetention(t *testing.T) {
	cfg, err := config.Load(writeServerTestConfig(t, mysqlDialectConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec, ok := notifyPruneSpec(cfg, myContract())
	if !ok {
		t.Fatal("MySQL worker specs do not schedule notify_prune")
	}
	if spec.Interval > mysqlstore.NotificationRetention || spec.Interval <= 0 {
		t.Fatalf("notify_prune interval = %v, want in (0, %v]", spec.Interval, mysqlstore.NotificationRetention)
	}
	if spec.Interval != time.Minute {
		t.Fatalf("notify_prune interval = %v, want %v", spec.Interval, time.Minute)
	}
}

// pgContract は PostgreSQL 構成の dbHandle から得た掃除の契約。接続は開かない。
// 登録と投入が使う値をそのまま得るため、テストでも dbHandle のメソッドを通す。
func pgContract() store.NotificationPruner {
	return (&dbHandle{pg: new(pgxpool.Pool)}).NotificationPruner()
}

// myContract は MySQL 構成の dbHandle から得た掃除の契約。接続は開かない。
func myContract() store.NotificationPruner {
	return (&dbHandle{my: new(sql.DB)}).NotificationPruner()
}

// fakeContract は契約の有無だけを見る行で使う、DB に接続しない契約。
// PostgreSQL 方言に契約を与えても投入されることで、判定が方言ではなく契約で決まることを示す。
var fakeContract store.NotificationPruner = mysqlstore.NewNotifications(nil)

// TestNotifyPruneFollowsContract は、notify_prune の投入が方言ではなく掃除の契約の有無で決まることを確かめる。
// dbHandle から得た契約（両方言）と、契約だけを与える行の両方向を置く。
// 両方向とは、契約が有れば投入し、無ければ投入しないこと。
func TestNotifyPruneFollowsContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfgText string
		pruner  store.NotificationPruner
		want    bool
	}{
		{"PostgreSQL 方言で dbHandle の契約（無し）", asynqDialectBaseConfig, pgContract(), false},
		{"MySQL 方言で dbHandle の契約（有り）", mysqlDialectConfig, myContract(), true},
		{"PostgreSQL 方言で契約が有る", asynqDialectBaseConfig, fakeContract, true},
		{"MySQL 方言で契約が無い", mysqlDialectConfig, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeServerTestConfig(t, tc.cfgText))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := hasNotifyPruneSpec(cfg, tc.pruner); got != tc.want {
				t.Fatalf("notify_prune scheduled = %v, want %v", got, tc.want)
			}
		})
	}
}

// notifyPruneSpec は定期投入の仕様から notify_prune を探す。pruner は構成の根が渡す契約（nil なら投入しない）。
// server.go と同じ入口（newWorkerClientConfig）を通し、pruner の受け渡しも検査対象にする。
func notifyPruneSpec(cfg *config.Config, pruner store.NotificationPruner) (jobq.PeriodicJobSpec, bool) {
	for _, spec := range newWorkerClientConfig(cfg, nil, nil, pruner, nil, false, 0).Spec.PeriodicJobs {
		if _, ok := spec.Args.(jobs.NotifyPruneArgs); ok {
			return spec, true
		}
	}
	return jobq.PeriodicJobSpec{}, false
}

// hasNotifyPruneSpec は notify_prune が定期投入の仕様に含まれるかを返す。
func hasNotifyPruneSpec(cfg *config.Config, pruner store.NotificationPruner) bool {
	_, ok := notifyPruneSpec(cfg, pruner)
	return ok
}
