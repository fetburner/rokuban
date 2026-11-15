package db

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuleImportKeysMigration(t *testing.T) {
	ctx := t.Context()
	dbURL := createScratchDatabase(t, testDatabaseURL(t), "rule_import_keys", "TEMPLATE template0")
	migrateUpTo(t, dbURL, 71)
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cases := []struct{ before, after, externalID string }{
		{`{"epgstation":{"ruleId":42.0}}`, `{}`, "42"},
		{`{"epgstation":{"ruleId":42.5}}`, `{"epgstation":{"ruleId":42.5}}`, ""},
		{`{"epgstation":{"ruleId":42.0000000000000000001}}`, `{"epgstation":{"ruleId":42.0000000000000000001}}`, ""},
		{`{"epgstation":{"ruleId":9223372036854775807}}`, `{}`, "9223372036854775807"},
		{`{"epgstation":{"ruleId":-9223372036854775808}}`, `{}`, "-9223372036854775808"},
		{`{"epgstation":{"ruleId":9223372036854775808}}`, `{"epgstation":{"ruleId":9223372036854775808}}`, ""},
		{`{"epgstation":{"ruleId":-9223372036854775809}}`, `{"epgstation":{"ruleId":-9223372036854775809}}`, ""},
		{`{"epgstation":{"ruleId":43.00,"other":true},"user":1}`, `{"epgstation":{"other":true},"user":1}`, "43"},
		{`{"epgstation":{"ruleId":4.4e1}}`, `{}`, "44"},
		{`{"epgstation":{"ruleId":"42"}}`, `{"epgstation":{"ruleId":"42"}}`, ""},
		{`{"epgstation":{"ruleId":null}}`, `{"epgstation":{"ruleId":null}}`, ""},
		{`{"epgstation":{"ruleId":true}}`, `{"epgstation":{"ruleId":true}}`, ""},
		{`{"epgstation":{"ruleId":[42]}}`, `{"epgstation":{"ruleId":[42]}}`, ""},
		{`{"epgstation":{"ruleId":{"x":42}}}`, `{"epgstation":{"ruleId":{"x":42}}}`, ""},
		{`{"user":1}`, `{"user":1}`, ""},
	}
	ids := make([]int64, len(cases))
	for i, tc := range cases {
		if err := pool.QueryRow(ctx, "INSERT INTO rules (name, metadata) VALUES ('legacy', $1::jsonb) RETURNING id", tc.before).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	migrateUpTo(t, dbURL, 72)
	for i, tc := range cases {
		var same bool
		if err := pool.QueryRow(ctx, "SELECT metadata = $2::jsonb FROM rules WHERE id=$1", ids[i], tc.after).Scan(&same); err != nil {
			t.Fatal(err)
		}
		if !same {
			t.Fatalf("metadata after migration for case %d differs from %s", i, tc.after)
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM rule_import_keys WHERE rule_id=$1 AND source='epgstation' AND external_id=$2", ids[i], tc.externalID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if tc.externalID != "" {
			want = 1
		}
		if count != want {
			t.Fatalf("key rows for case %d = %d, want %d", i, count, want)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM rule_import_keys").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("migrated keys = %d, want 5", count)
	}
	migrateDownTo(t, dbURL, 71)
	for i, tc := range cases {
		var same bool
		if err := pool.QueryRow(ctx, "SELECT metadata = $2::jsonb FROM rules WHERE id=$1", ids[i], tc.before).Scan(&same); err != nil {
			t.Fatal(err)
		}
		if !same {
			t.Fatalf("Down did not restore metadata for case %d", i)
		}
	}

}

func TestRuleImportKeysMigrationRejectsDuplicates(t *testing.T) {
	ctx := t.Context()
	dbURL := createScratchDatabase(t, testDatabaseURL(t), "duplicate_rule_keys", "TEMPLATE template0")
	migrateUpTo(t, dbURL, 71)
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, metadata := range []string{`{"epgstation":{"ruleId":42}}`, `{"epgstation":{"ruleId":42.0}}`} {
		if _, err := pool.Exec(ctx, "INSERT INTO rules (name, metadata) VALUES ('duplicate', $1::jsonb)", metadata); err != nil {
			t.Fatal(err)
		}
	}
	err = MigrateUp(ctx, dbURL)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("migration error = %v, want unique violation", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM rules WHERE metadata <> '{}'::jsonb").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("failed migration changed metadata: remaining=%d", count)
	}
	var tableExists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('rule_import_keys') IS NOT NULL").Scan(&tableExists); err != nil {
		t.Fatal(err)
	}
	if tableExists {
		t.Fatal("failed migration left rule_import_keys behind")
	}
}
