package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestMediaAssetCopiesMigrationBackfillsCopiesAndGenerations(t *testing.T) {
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
		t.Fatalf("clearing prior media fixtures: %v", err)
	}
	pool.Close()
	migrateDownTo(t, dbURL, 36)
	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Errorf("opening test database for cleanup: %v", err)
			return
		}
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing migration fixtures: %v", err)
		}
		cleanupPool.Close()
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()

	type legacyAsset struct {
		id      int64
		kind    string
		relPath string
		state   string
	}
	cutProfile := "cut"
	fixtures := []struct {
		kind, path, state string
		profile           *string
	}{
		{kind: "original", path: "sites/default/original.m2ts", state: "active"},
		{kind: "encoded", profile: &cutProfile, path: "sites/default/program_cut.g7.mp4", state: "active"},
		{kind: "thumbnail", path: "thumbnails/7.g12.jpg", state: "deleted"},
		{kind: "seek_tiles", path: "thumbnails/7_tiles.jpg", state: "active"},
	}
	assets := make([]legacyAsset, 0, len(fixtures))
	for i, fixture := range fixtures {
		recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
			p.EventID = int32(51000 + i)
		})
		var id int64
		var deletedAt *time.Time
		if fixture.state == "deleted" {
			at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			deletedAt = &at
		}
		if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state, deleted_at)
VALUES ($1, $2, $3, $4, 10, $5, $6)
RETURNING id`, recordingID, fixture.kind, fixture.profile, fixture.path, fixture.state, deletedAt).Scan(&id); err != nil {
			t.Fatalf("inserting legacy %s asset: %v", fixture.kind, err)
		}
		assets = append(assets, legacyAsset{id: id, kind: fixture.kind, relPath: fixture.path, state: fixture.state})
	}

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying media asset copies migration: %v", err)
	}

	// Old workers keep using the former single-column conflict targets during a
	// rolling update. Each statement must remain valid after the composite keys
	// are installed.
	legacyOrphanPath := fmt.Sprintf("legacy-writer-%d.m2ts", assets[0].id)
	for range 2 {
		if _, err := pool.Exec(ctx, `INSERT INTO orphan_files (rel_path) VALUES ($1) ON CONFLICT (rel_path) DO NOTHING`, legacyOrphanPath); err != nil {
			t.Fatalf("legacy orphan upsert with old conflict target: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO missing_media_assets (media_asset_id) VALUES ($1) ON CONFLICT (media_asset_id) DO NOTHING`, assets[0].id); err != nil {
			t.Fatalf("legacy missing-asset upsert with old conflict target: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes, observed_at)
VALUES ('media', '/legacy', 100, 25, 75, now())
ON CONFLICT (root) DO UPDATE SET path = EXCLUDED.path`); err != nil {
			t.Fatalf("legacy storage sync upsert with old conflict target: %v", err)
		}
	}
	var legacyOrphanCount, legacyMissingCount, legacyStorageCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orphan_files WHERE rel_path = $1`, legacyOrphanPath).Scan(&legacyOrphanCount); err != nil {
		t.Fatalf("counting old-key orphan row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1`, assets[0].id).Scan(&legacyMissingCount); err != nil {
		t.Fatalf("counting old-key missing row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM storage_sync WHERE root = 'media'`).Scan(&legacyStorageCount); err != nil {
		t.Fatalf("counting old-key storage row: %v", err)
	}
	if legacyOrphanCount != 1 || legacyMissingCount != 1 || legacyStorageCount != 1 {
		t.Fatalf("legacy conflict target rows = orphan %d / missing %d / storage %d, want 1 / 1 / 1", legacyOrphanCount, legacyMissingCount, legacyStorageCount)
	}

	for _, asset := range assets {
		var generation *int64
		if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, asset.id).Scan(&generation); err != nil {
			t.Fatalf("reading derived generation for %s: %v", asset.kind, err)
		}
		switch asset.kind {
		case "original":
			if generation != nil {
				t.Errorf("original derived_generation = %d, want NULL", *generation)
			}
		case "encoded":
			if generation == nil || *generation != 7 {
				t.Errorf("encoded derived_generation = %v, want 7", generation)
			}
		case "thumbnail":
			if generation == nil || *generation != 12 {
				t.Errorf("deleted thumbnail derived_generation = %v, want 12", generation)
			}
		case "seek_tiles":
			if generation == nil || *generation != 0 {
				t.Errorf("unversioned seek_tiles derived_generation = %v, want 0", generation)
			}
		}

		var copyStore, copyRelPath, copyState string
		var copyDeletedAt *time.Time
		if err := pool.QueryRow(ctx, `
SELECT store, rel_path, state, deleted_at FROM media_asset_copies WHERE media_asset_id = $1`, asset.id).
			Scan(&copyStore, &copyRelPath, &copyState, &copyDeletedAt); err != nil {
			t.Fatalf("reading backfilled copy for %s: %v", asset.kind, err)
		}
		if copyStore != "" || copyRelPath != asset.relPath || copyState != asset.state {
			t.Errorf("backfilled copy for %s = %q/%q/%q, want blank store and legacy path/state %q/%q", asset.kind, copyStore, copyRelPath, copyState, asset.relPath, asset.state)
		}
		if asset.state == "deleted" && copyDeletedAt == nil {
			t.Errorf("backfilled deleted copy for %s has no deleted_at", asset.kind)
		}
	}

	var copyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies`).Scan(&copyCount); err != nil {
		t.Fatalf("counting backfilled copies: %v", err)
	}
	if copyCount != len(assets) {
		t.Fatalf("backfilled copies = %d, want %d", copyCount, len(assets))
	}
}
