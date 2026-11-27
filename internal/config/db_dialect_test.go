package config

import (
	"strings"
	"testing"
)

// dialectConfig は minimalConfig に db.dialect と追加の db・worker のキーを足した設定を返す。
func dialectConfig(dbExtra, workerExtra string) string {
	return strings.Replace(minimalConfig, "db:\n", "db:\n"+dbExtra, 1) + workerExtra
}

// TestLoad_DialectDefaultsToPostgres は、方言を書かない設定（asynq と組む）が PostgreSQL として読まれ、
// ポートが PostgreSQL の既定（5432）になることを確かめる。既定を mysql に変えると、この値の検査が落ちる。
func TestLoad_DialectDefaultsToPostgres(t *testing.T) {
	cfg, err := Load(writeConfig(t, dialectConfig("", "worker:\n  engine: asynq\n  redis_url: redis://localhost:6379/0\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Dialect != DBDialectPostgres {
		t.Errorf("db.dialect = %q, want %q", cfg.DB.Dialect, DBDialectPostgres)
	}
	if cfg.DB.Port != 5432 {
		t.Errorf("db.port = %d, want 5432", cfg.DB.Port)
	}
}

// TestLoad_MySQLRequiresAsynq は、MySQL 方言で River を選ぶ設定を起動時に拒むことを確かめる。
// River は PostgreSQL 専用なので、既定の worker.engine（river）のままでは MySQL 構成は起動できない。
func TestLoad_MySQLRequiresAsynq(t *testing.T) {
	path := writeConfig(t, dialectConfig("  dialect: mysql\n", ""))
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "requires worker.engine: asynq") {
		t.Fatalf("Load error = %v, want rejection requiring worker.engine: asynq", err)
	}
}

// TestLoad_MySQLWithAsynqDefaultsPort は、asynq の MySQL 構成が通り、ポートが MySQL の既定（3306）になることを確かめる。
func TestLoad_MySQLWithAsynqDefaultsPort(t *testing.T) {
	path := writeConfig(t, dialectConfig("  dialect: mysql\n", "worker:\n  engine: asynq\n  redis_url: redis://localhost:6379/0\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Dialect != DBDialectMySQL {
		t.Errorf("db.dialect = %q, want %q", cfg.DB.Dialect, DBDialectMySQL)
	}
	if cfg.DB.Port != 3306 {
		t.Errorf("db.port = %d, want 3306", cfg.DB.Port)
	}
}

// TestLoad_MySQLRejectsPostgresOnlyKeys は、MySQL で TLS と statement timeout を黙って無視せず拒むことを確かめる。
func TestLoad_MySQLRejectsPostgresOnlyKeys(t *testing.T) {
	const asynq = "worker:\n  engine: asynq\n  redis_url: redis://localhost:6379/0\n"
	tests := []struct {
		name string
		db   string
		want string
	}{
		{"sslmode", "  dialect: mysql\n  sslmode: require\n", "db.sslmode must be disable"},
		{"api_statement_timeout", "  dialect: mysql\n  api_statement_timeout: 10s\n", "db.api_statement_timeout is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, dialectConfig(tt.db, asynq)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestLoad_UnknownDialectRejected は未知の方言を拒むことを確かめる。
func TestLoad_UnknownDialectRejected(t *testing.T) {
	_, err := Load(writeConfig(t, dialectConfig("  dialect: oracle\n", "")))
	if err == nil || !strings.Contains(err.Error(), "db.dialect must be postgres or mysql") {
		t.Fatalf("Load error = %v, want unknown dialect rejection", err)
	}
}
