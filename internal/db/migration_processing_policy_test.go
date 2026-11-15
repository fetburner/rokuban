package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

// TestProcessingPolicyMigrationPreservesPolicy は改名前の方針と制約の継承を検証する。
func TestProcessingPolicyMigrationPreservesPolicy(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE recordings CASCADE`); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	migrateDownTo(t, dbURL, 65)
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
			return
		}
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Error(err)
		}
	})
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	id := dbtest.Recording(t, pool)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO recording_encode_policy
(recording_id, keep_original, encode_profiles, cm_detect, cm_detect_generation, created_at, updated_at)
VALUES ($1, 'until_encoded', ARRAY['h264', 'h265'], true, 7, $2, $2)`, id, stamp); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var keep string
	var profiles []string
	var enabled bool
	var generation int64
	var created, updated time.Time
	if err := pool.QueryRow(ctx, `SELECT keep_original, encode_profiles, cm_detect,
cm_detect_generation, created_at, updated_at FROM recording_processing_policy WHERE recording_id = $1`, id).
		Scan(&keep, &profiles, &enabled, &generation, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if keep != "until_encoded" || !slices.Equal(profiles, []string{"h264", "h265"}) || !enabled || generation != 7 || !created.Equal(stamp) || !updated.Equal(stamp) {
		t.Fatalf("migrated policy = %q %v %v %d %v %v", keep, profiles, enabled, generation, created, updated)
	}
	for _, query := range []string{
		`UPDATE recording_processing_policy SET encode_profiles = '{}' WHERE recording_id = $1`,
		`UPDATE recording_processing_policy SET keep_original = 'invalid' WHERE recording_id = $1`,
	} {
		_, err := pool.Exec(ctx, query, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("constraint violation = %v; want SQLSTATE 23514", err)
		}
	}
	// SQL 本文の旧表名が残っていれば、関数の実行時の名前解決で失敗する。
	if _, err := pool.Exec(ctx, `SELECT * FROM until_encoded_deletable_originals('migration-test')`); err != nil {
		t.Fatalf("calling migrated retention predicate: %v", err)
	}
}
