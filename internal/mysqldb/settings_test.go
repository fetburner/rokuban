package mysqldb_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/mysqltest"
)

func TestOpenPoolEnforcesConnectionSettings(t *testing.T) {
	db, err := mysqldb.OpenPool(context.Background(), mysqltest.TestDSN(t))
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing MySQL pool: %v", err)
		}
	})

	var isolation, sqlMode, collation, timeZone string
	if err := db.QueryRowContext(context.Background(), `
		SELECT @@transaction_isolation, @@sql_mode, @@collation_connection, @@time_zone
	`).Scan(&isolation, &sqlMode, &collation, &timeZone); err != nil {
		t.Fatalf("reading MySQL connection settings: %v", err)
	}
	if isolation != "READ-COMMITTED" {
		t.Errorf("transaction_isolation = %q, want READ-COMMITTED", isolation)
	}
	modeSet := make(map[string]struct{})
	for _, mode := range strings.Split(sqlMode, ",") {
		modeSet[mode] = struct{}{}
	}
	for _, mode := range []string{
		"TRADITIONAL",
		"STRICT_TRANS_TABLES",
		"STRICT_ALL_TABLES",
		"NO_ZERO_IN_DATE",
		"NO_ZERO_DATE",
		"ERROR_FOR_DIVISION_BY_ZERO",
		"NO_ENGINE_SUBSTITUTION",
	} {
		if _, ok := modeSet[mode]; !ok {
			t.Errorf("sql_mode %q does not contain %q", sqlMode, mode)
		}
	}
	if collation != "utf8mb4_0900_as_cs" {
		t.Errorf("collation_connection = %q, want utf8mb4_0900_as_cs", collation)
	}
	if timeZone != "+00:00" {
		t.Errorf("time_zone = %q, want +00:00", timeZone)
	}
}

func TestCheckConnectionSettingsRejectsEachMismatch(t *testing.T) {
	dsn := mysqltest.TestDSN(t)
	cases := []struct {
		name       string
		parameter  string
		value      string
		wantErrMsg string
	}{
		{
			name:       "isolation",
			parameter:  "transaction_isolation",
			value:      "'REPEATABLE-READ'",
			wantErrMsg: `MySQL transaction_isolation "REPEATABLE-READ" does not match required setting`,
		},
		{
			name:       "sql_mode",
			parameter:  "sql_mode",
			value:      "'ANSI'",
			wantErrMsg: "MySQL sql_mode does not satisfy TRADITIONAL mode requirements",
		},
		{
			name:       "collation",
			parameter:  "collation_connection",
			value:      "'utf8mb4_0900_ai_ci'",
			wantErrMsg: `MySQL collation_connection "utf8mb4_0900_ai_ci" does not match required setting`,
		},
		{
			name:       "time_zone",
			parameter:  "time_zone",
			value:      "'+09:00'",
			wantErrMsg: `MySQL time_zone "+09:00" does not match required setting`,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := mysql.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("parsing MySQL DSN: %v", err)
			}
			if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
				t.Fatalf("setting test connection charset: %v", err)
			}
			cfg.Params = map[string]string{
				"transaction_isolation": "'READ-COMMITTED'",
				"sql_mode":              "'TRADITIONAL'",
				"collation_connection":  "'utf8mb4_0900_as_cs'",
				"time_zone":             "'+00:00'",
			}
			cfg.Params[tt.parameter] = tt.value
			db, err := sql.Open("mysql", cfg.FormatDSN())
			if err != nil {
				t.Fatalf("opening misconfigured MySQL pool: %v", err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Errorf("closing MySQL pool: %v", err)
				}
			})
			if err := db.PingContext(context.Background()); err != nil {
				t.Fatalf("connecting with %s mismatch: %v", tt.name, err)
			}
			if err := mysqldb.CheckConnectionSettings(context.Background(), db); err == nil || err.Error() != tt.wantErrMsg {
				t.Fatalf("CheckConnectionSettings error = %v, want %q", err, tt.wantErrMsg)
			}
		})
	}
}
