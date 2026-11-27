package main

import (
	"strings"
	"testing"
	"time"

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
	if hasNotifyPruneSpec(cfg, nil) {
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
	spec, ok := notifyPruneSpec(cfg, notifyContract)
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

// notifyContract は MySQL 構成が登録と投入に渡す通知表の掃除の契約。DB には接続しない。
var notifyContract store.NotificationPruner = mysqlstore.NewNotifications(nil)

// TestNotifyPruneFollowsContract は、notify_prune の投入が方言ではなく掃除の契約の有無で決まることを確かめる。
// 方言と契約の組み合わせを両方向に置く。
func TestNotifyPruneFollowsContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfgText string
		pruner  store.NotificationPruner
		want    bool
	}{
		{"PostgreSQL 方言で契約が有る", asynqDialectBaseConfig, notifyContract, true},
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
func notifyPruneSpec(cfg *config.Config, pruner store.NotificationPruner) (jobq.PeriodicJobSpec, bool) {
	for _, spec := range workerPeriodicJobSpecs(cfg, nil, nil, pruner) {
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
