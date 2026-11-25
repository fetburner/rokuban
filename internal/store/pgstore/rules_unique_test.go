package pgstore

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRuleImportKeysConstraints(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	var first, second int64
	for _, id := range []*int64{&first, &second} {
		if err := pool.QueryRow(ctx, "INSERT INTO rules (name) VALUES ('test') RETURNING id").Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '42')", first); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '42')",
		"INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '')",
		"INSERT INTO rule_import_keys VALUES ($1, 'unknown', '43')",
	} {
		if _, err := pool.Exec(ctx, query, second); err == nil {
			t.Fatalf("accepted invalid key: %s", query)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '43')", second); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM rules WHERE id = $1", first); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM rule_import_keys").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("keys after cascade = %d, want 1", count)
	}
}

func TestRuleImportPreservesConcurrentMetadata(t *testing.T) {
	pool := testutil.SetupDB(t)
	rules := NewRules(pool, []string{"default"})
	ctx := t.Context()
	if _, err := rules.ImportEPGStationRule(ctx, 42, store.RuleInput{Name: "original"}); err != nil {
		t.Fatal(err)
	}
	rows, err := rules.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `UPDATE rules SET metadata='{"manual":"concurrent"}'::jsonb WHERE id=$1`, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := rules.ImportEPGStationRule(ctx, 42, store.RuleInput{Name: "updated"}); done <- err }()
	waitForRuleWriters(t, pool, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("import did not complete")
	}
	after, err := rules.Get(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata["manual"] != "concurrent" {
		t.Fatalf("metadata after concurrent write = %v", after.Metadata)
	}
}
