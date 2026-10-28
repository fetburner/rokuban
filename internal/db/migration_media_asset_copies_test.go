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
		id             int64
		kind           string
		relPath        string
		state          string
		wantGeneration int64
		generationNull bool
	}
	cutProfile := "cut"
	dottedProfile := "x.g2.hd"
	uncutProfile := "h264"
	fixtures := []struct {
		kind, path, state string
		profile           *string
		wantGeneration    int64
		generationNull    bool
	}{
		{kind: "original", path: "sites/default/original.m2ts", state: "active", generationNull: true},
		{kind: "encoded", profile: &cutProfile, path: "sites/default/program_cut.g7.mp4", state: "active", wantGeneration: 7},
		{kind: "encoded", profile: &dottedProfile, path: "sites/default/program_x.g2.hd.g1.mp4", state: "active", wantGeneration: 1},
		{kind: "encoded", profile: &uncutProfile, path: "sites/default/program_h264.u9.mp4", state: "active", wantGeneration: 9},
		{kind: "encoded", profile: &cutProfile, path: "sites/default/program_cut.g99999999999999999999.mp4", state: "active", wantGeneration: 0},
		{kind: "thumbnail", path: "thumbnails/7.g12.jpg", state: "deleted", wantGeneration: 12},
		{kind: "seek_tiles", path: "thumbnails/7_tiles.jpg", state: "active", wantGeneration: 0},
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
		assets = append(assets, legacyAsset{
			id: id, kind: fixture.kind, relPath: fixture.path, state: fixture.state,
			wantGeneration: fixture.wantGeneration, generationNull: fixture.generationNull,
		})
	}

	migrateUpTo(t, dbURL, 39)
	for _, asset := range assets {
		assertMigratedAssetGeneration(t, pool, asset.id, asset.relPath, asset.wantGeneration, asset.generationNull)

		// 00037 leaves logical assets unassigned; the compatibility release's
		// storage adopt creates their physical copy rows before 00040 removes these columns.
		var copies int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, asset.id).Scan(&copies); err != nil {
			t.Fatalf("counting pre-adoption copies for %s: %v", asset.kind, err)
		}
		if copies != 0 {
			t.Errorf("%s has %d copy row(s) before adoption, want none", asset.kind, copies)
		}
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

	if err := adoptLegacyMediaAssetsForTest(ctx, pool); err != nil {
		t.Fatalf("adopting legacy media asset copies before dropping their source columns: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying media asset copy compatibility cleanup: %v", err)
	}
	assertMediaAssetCopyColumnsRetired(t, pool)

	for _, asset := range assets {
		var relPath, state string
		if err := pool.QueryRow(ctx, `
SELECT rel_path, state
FROM media_asset_copies
WHERE media_asset_id = $1 AND store = 'migration-test'`, asset.id).Scan(&relPath, &state); err != nil {
			t.Fatalf("reading adopted %s copy after migration: %v", asset.kind, err)
		}
		if relPath != asset.relPath || state != asset.state {
			t.Errorf("%s copy = (%q, %q), want (%q, %q)", asset.kind, relPath, state, asset.relPath, asset.state)
		}
	}
}

func assertMediaAssetCopyColumnsRetired(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, column := range []string{"rel_path", "state", "deleted_at"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public' AND table_name = 'media_assets' AND column_name = $1
)`, column).Scan(&exists); err != nil {
			t.Fatalf("checking media_assets.%s: %v", column, err)
		}
		if exists {
			t.Errorf("media_assets.%s should not exist after compatibility cleanup", column)
		}
	}
	for _, index := range []string{"media_assets_kind_state_idx", "media_assets_rel_path_idx"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, index).Scan(&exists); err != nil {
			t.Fatalf("checking %s: %v", index, err)
		}
		if exists {
			t.Errorf("%s should not exist after compatibility cleanup", index)
		}
	}
}

func TestMediaAssetCopyCleanupRefusesUnadoptedAssets(t *testing.T) {
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
	migrateDownTo(t, dbURL, 39)
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
	recordingID := dbtest.Recording(t, pool)
	var assetID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'original', 'sites/default/unadopted.m2ts', 1)
RETURNING id`, recordingID).Scan(&assetID); err != nil {
		t.Fatalf("inserting unadopted legacy asset: %v", err)
	}

	if err := MigrateUp(ctx, dbURL); err == nil {
		t.Fatal("migration succeeded with a logical asset that had no copy")
	}
	var stillHasLegacyPath bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public' AND table_name = 'media_assets' AND column_name = 'rel_path'
)`).Scan(&stillHasLegacyPath); err != nil {
		t.Fatalf("checking rollback after blocked migration: %v", err)
	}
	if !stillHasLegacyPath {
		t.Fatal("blocked migration removed rel_path before adoption completed")
	}

	if err := adoptLegacyMediaAssetsForTest(ctx, pool); err != nil {
		t.Fatalf("adopting fixture before retrying migration: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migration failed after adoption: %v", err)
	}
	assertMediaAssetCopyColumnsRetired(t, pool)
}

func assertMigratedAssetGeneration(t *testing.T, pool *pgxpool.Pool, assetID int64, relPath string, want int64, wantNull bool) {
	t.Helper()
	var got *int64
	if err := pool.QueryRow(context.Background(), `SELECT derived_generation FROM media_assets WHERE id = $1`, assetID).Scan(&got); err != nil {
		t.Fatalf("reading derived generation for %s: %v", relPath, err)
	}
	if wantNull {
		if got != nil {
			t.Errorf("%s derived_generation = %d, want NULL", relPath, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s derived_generation is NULL, want %d", relPath, want)
	} else if *got != want {
		t.Errorf("%s derived_generation = %d, want %d", relPath, *got, want)
	}
}
