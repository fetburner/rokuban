package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecordingsUnpurgedIDIndexMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	migrateDownTo(t, dbURL, 59)
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying recordings unpurged ID index migration: %v", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening migrated test database: %v", err)
	}
	var oldExists, newExists bool
	if err := pool.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'recordings_purged_at_idx'),
			EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'recordings_unpurged_id_idx')`,
	).Scan(&oldExists, &newExists); err != nil {
		pool.Close()
		t.Fatalf("checking recordings unpurged indexes: %v", err)
	}
	if oldExists {
		t.Error("recordings_purged_at_idx should not exist")
	}
	if !newExists {
		pool.Close()
		t.Fatal("recordings_unpurged_id_idx does not exist")
	}

	var key, predicate string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_indexdef(i.indexrelid, 1, true), pg_get_expr(i.indpred, i.indrelid)
		FROM pg_index i
		WHERE i.indexrelid = 'public.recordings_unpurged_id_idx'::regclass`,
	).Scan(&key, &predicate); err != nil {
		pool.Close()
		t.Fatalf("reading recordings_unpurged_id_idx definition: %v", err)
	}
	if key != "id" {
		t.Errorf("recordings_unpurged_id_idx key = %q, want id", key)
	}
	normalizedPredicate := strings.Join(strings.Fields(strings.NewReplacer("(", "", ")", "").Replace(predicate)), " ")
	if normalizedPredicate != "purged_at IS NULL" {
		t.Errorf("recordings_unpurged_id_idx predicate = %q, want purged_at IS NULL", predicate)
	}
	pool.Close()

	migrateDownTo(t, dbURL, 59)
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening downgraded test database: %v", err)
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'recordings_purged_at_idx'),
			EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'recordings_unpurged_id_idx')`,
	).Scan(&oldExists, &newExists); err != nil {
		t.Fatalf("checking rolled-back recordings unpurged indexes: %v", err)
	}
	if !oldExists || newExists {
		t.Errorf("indexes after rollback: old exists = %t, new exists = %t; want old only", oldExists, newExists)
	}
}
