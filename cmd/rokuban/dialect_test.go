package main

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
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

// mysqlDialectConfig は db.dialect: mysql と asynq の設定（MySQL 構成が起動を許される唯一の組み合わせ）。
var mysqlDialectConfig = strings.Replace(dialectBaseConfig, "db:\n", "db:\n  dialect: mysql\n", 1) + `
worker:
  engine: asynq
  redis_url: redis://localhost:6379/7
`

// TestDefaultDialectAssemblesPostgresStack は、db.dialect を書かない設定が PostgreSQL の組み立てに
// なることを確かめる。既存の設定を変えないための既定である。config の既定を MySQL にすると、この値の検査が落ちる。
func TestDefaultDialectAssemblesPostgresStack(t *testing.T) {
	cfg, err := config.Load(writeServerTestConfig(t, dialectBaseConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := stackDialect(cfg); got != config.DBDialectPostgres {
		t.Fatalf("stackDialect() = %q for a config without db.dialect, want %q", got, config.DBDialectPostgres)
	}
	if hasNotifyPruneSpec(cfg) {
		t.Fatal("PostgreSQL worker specs must not schedule notify_prune")
	}
}

// TestMySQLDialectAssemblesMySQLStack は、db.dialect: mysql（asynq）の設定が MySQL の組み立てになることを確かめる。
func TestMySQLDialectAssemblesMySQLStack(t *testing.T) {
	cfg, err := config.Load(writeServerTestConfig(t, mysqlDialectConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := stackDialect(cfg); got != config.DBDialectMySQL {
		t.Fatalf("stackDialect() = %q, want %q", got, config.DBDialectMySQL)
	}
}

// TestNotifyPruneIntervalWithinRetention は、MySQL の掃除ジョブが保持期間以下の間隔で定期投入され、
// PostgreSQL の定期ジョブには現れないことを確かめる。
func TestNotifyPruneIntervalWithinRetention(t *testing.T) {
	cfg, err := config.Load(writeServerTestConfig(t, mysqlDialectConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec, ok := notifyPruneSpec(cfg)
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

// notifyPruneSpec は定期投入の仕様から notify_prune を探す。
func notifyPruneSpec(cfg *config.Config) (jobq.PeriodicJobSpec, bool) {
	for _, spec := range workerPeriodicJobSpecs(cfg, nil, nil) {
		if _, ok := spec.Args.(jobs.NotifyPruneArgs); ok {
			return spec, true
		}
	}
	return jobq.PeriodicJobSpec{}, false
}

// hasNotifyPruneSpec は notify_prune が定期投入の仕様に含まれるかを返す。
func hasNotifyPruneSpec(cfg *config.Config) bool {
	_, ok := notifyPruneSpec(cfg)
	return ok
}
