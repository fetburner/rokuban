package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestMediaAssetCopyGenerationMigrationBackfillsAndAllowsOverlap(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database before setup: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE recordings CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("clearing prior fixtures: %v", err)
	}
	pool.Close()
	migrateDownTo(t, dbURL, 41)
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
			return
		}
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Errorf("opening test database for cleanup: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing migration fixtures: %v", err)
		}
	})

	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening version 39 test database: %v", err)
	}
	recordingID := dbtest.Recording(t, pool)
	var assetID, oldCopyID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state, derived_generation)
VALUES ($1, 'encoded', 'h264', 'encoded/h264.g7.mp4', 10, 'active', 7)
RETURNING id`, recordingID).Scan(&assetID); err != nil {
		pool.Close()
		t.Fatalf("seeding legacy encoded asset: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at)
VALUES ($1, 'test-store', 'encoded/h264.g7.mp4', 'active', $2)
RETURNING id`, assetID, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)).Scan(&oldCopyID); err != nil {
		pool.Close()
		t.Fatalf("seeding legacy physical copy: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO missing_media_assets (media_asset_id, store, first_seen) VALUES ($1, 'test-store', $2)`, assetID, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)); err != nil {
		pool.Close()
		t.Fatalf("seeding legacy missing-copy observation: %v", err)
	}
	pool.Close()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying copy-generation migration: %v", err)
	}
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening migrated test database: %v", err)
	}
	defer pool.Close()

	var generation int64
	if err := pool.QueryRow(ctx, `SELECT generation FROM media_asset_copies WHERE id = $1`, oldCopyID).Scan(&generation); err != nil {
		t.Fatalf("reading migrated copy generation: %v", err)
	}
	if generation != 7 {
		t.Fatalf("migrated copy generation = %d, want 7 from rel_path", generation)
	}
	var missingCopyID int64
	if err := pool.QueryRow(ctx, `SELECT media_asset_copy_id FROM missing_media_assets WHERE media_asset_copy_id = $1`, oldCopyID).Scan(&missingCopyID); err != nil {
		t.Fatalf("reading migrated missing-copy observation: %v", err)
	}
	if missingCopyID != oldCopyID {
		t.Fatalf("missing observation copy ID = %d, want %d", missingCopyID, oldCopyID)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, 'test-store', 'encoded/h264.g8.mp4', 8, 'active')`, assetID); err != nil {
		t.Fatalf("inserting overlapping generation in the same store: %v", err)
	}
	var copies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'test-store'`, assetID).Scan(&copies); err != nil {
		t.Fatalf("counting same-store generations: %v", err)
	}
	if copies != 2 {
		t.Fatalf("same-store copy rows = %d, want old and new physical files", copies)
	}
}
