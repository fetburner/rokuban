package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

// TestTransferWriterGenerationMigrationPreservesProgress は転送中の進捗と制約・索引の継承を検証する。
func TestTransferWriterGenerationMigrationPreservesProgress(t *testing.T) {
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
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := MigrateUp(cleanupCtx, dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
			return
		}
		cleanupPool, err := pgxpool.New(cleanupCtx, dbURL)
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `TRUNCATE recordings CASCADE`); err != nil {
			t.Error(err)
			return
		}
		var hasCopies, hasProgress bool
		if err := cleanupPool.QueryRow(cleanupCtx, `SELECT
    EXISTS (SELECT 1 FROM media_asset_copies),
    EXISTS (SELECT 1 FROM media_asset_copy_transfer_progress)`).Scan(&hasCopies, &hasProgress); err != nil {
			t.Error(err)
			return
		}
		if hasCopies || hasProgress {
			t.Errorf("TRUNCATE recordings CASCADE left copies=%t progress=%t; want both empty", hasCopies, hasProgress)
		}
	})
	migrateDownTo(t, dbURL, 68)
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	var copyID int64
	if err := pool.QueryRow(ctx, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state) VALUES ($1, 'archive', 'transfer.ts', 0, 'pending') RETURNING id`, assetID).Scan(&copyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_asset_copy_transfer_progress (copy_id, lease_owner, lease_expires_at, generation, handed_off, offset_bytes, hash_state, upload_store, upload_state) VALUES ($1, '00000000-0000-0000-0000-000000000001', '2099-01-01', 7, true, 123, '\x0102', 'archive', '\x0304')`, copyID); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(p) - 'generation')::text FROM media_asset_copy_transfer_progress p WHERE copy_id=$1`, copyID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// OID の保持により、既存の索引と制約を作り直さず引き継いだことも確認する。
	const objects = `SELECT array_agg(oid::bigint ORDER BY oid) FROM (SELECT oid FROM pg_constraint WHERE conrelid='media_asset_copy_transfer_progress'::regclass UNION ALL SELECT indexrelid FROM pg_index WHERE indrelid='media_asset_copy_transfer_progress'::regclass) s`
	var oldObjects, newObjects []int64
	if err := pool.QueryRow(ctx, objects).Scan(&oldObjects); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	const constraintNameQuery = `SELECT conname FROM pg_constraint
WHERE conrelid = 'media_asset_copy_transfer_progress'::regclass
  AND contype = 'c'
  AND pg_get_constraintdef(oid) LIKE '%writer_generation%'`
	var constraintName string
	if err := pool.QueryRow(ctx, constraintNameQuery).Scan(&constraintName); err != nil {
		t.Fatal(err)
	}
	if constraintName != "media_asset_copy_transfer_progress_writer_generation_check" {
		t.Fatalf("migrated constraint name = %q", constraintName)
	}
	var after string
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(p) - 'writer_generation')::text, writer_generation FROM media_asset_copy_transfer_progress p WHERE copy_id=$1`, copyID).Scan(&after, &generation); err != nil {
		t.Fatal(err)
	}
	if before != after || generation != 7 {
		t.Fatalf("progress changed: before=%s after=%s writer_generation=%d", before, after, generation)
	}
	if err := pool.QueryRow(ctx, objects).Scan(&newObjects); err != nil {
		t.Fatal(err)
	}
	if len(oldObjects) != len(newObjects) {
		t.Fatalf("objects: %v -> %v", oldObjects, newObjects)
	}
	for i := range oldObjects {
		if oldObjects[i] != newObjects[i] {
			t.Fatalf("objects: %v -> %v", oldObjects, newObjects)
		}
	}
	for _, query := range []string{
		`UPDATE media_asset_copy_transfer_progress SET writer_generation=0 WHERE copy_id=$1`,
		`UPDATE media_asset_copy_transfer_progress SET offset_bytes=-1 WHERE copy_id=$1`,
		`UPDATE media_asset_copy_transfer_progress SET upload_store=NULL WHERE copy_id=$1`,
		`UPDATE media_asset_copy_transfer_progress SET verifying_source=true WHERE copy_id=$1`,
	} {
		_, err := pool.Exec(ctx, query, copyID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("constraint violation = %v; want 23514", err)
		}
	}
	migrateDownTo(t, dbURL, 68)
	if err := pool.QueryRow(ctx, `SELECT generation FROM media_asset_copy_transfer_progress WHERE copy_id=$1`, copyID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT conname FROM pg_constraint
WHERE conrelid = 'media_asset_copy_transfer_progress'::regclass
  AND contype = 'c'
  AND pg_get_constraintdef(oid) LIKE '%generation%'`).Scan(&constraintName); err != nil {
		t.Fatal(err)
	}
	if constraintName != "media_asset_copy_transfer_progress_generation_check" {
		t.Fatalf("rollback constraint name = %q", constraintName)
	}
	if generation != 7 {
		t.Fatalf("rollback generation=%d; want 7", generation)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
}
