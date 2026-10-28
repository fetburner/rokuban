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
		assertMigratedAssetGeneration(t, pool, asset.id, asset.relPath, asset.wantGeneration, asset.generationNull)

		// コピー行が無いことが未割り当てを表す。マイグレーションは行を作らない。
		var copies int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, asset.id).Scan(&copies); err != nil {
			t.Fatalf("counting copies for %s: %v", asset.kind, err)
		}
		if copies != 0 {
			t.Errorf("%s has %d copy row(s) after migration, want none until storage adopt", asset.kind, copies)
		}
	}

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

func TestMediaAssetCopiesPermitPendingReplacementForSameStore(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE recordings CASCADE`); err != nil {
		t.Fatalf("clearing media fixtures: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing media fixtures: %v", err)
		}
	}()

	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.Kind = "encoded"
		params.Profile = &profile
		params.RelPath = "encoded/old.mp4"
	})
	if _, err := pool.Exec(ctx, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
VALUES ($1, $2, 'encoded/new.mp4', 'pending')`, assetID, dbtest.StoreName); err != nil {
		t.Fatalf("inserting pending replacement beside active copy: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
VALUES ($1, $2, 'encoded/second-active.mp4', 'active')`, assetID, dbtest.StoreName); err == nil {
		t.Fatal("second active copy for the same asset and store was accepted")
	}

	if _, err := pool.Exec(ctx, `TRUNCATE recordings CASCADE`); err != nil {
		t.Fatalf("clearing replacement fixtures before downgrade: %v", err)
	}
	migrateDownTo(t, dbURL, 39)
	var unique bool
	if err := pool.QueryRow(ctx, `SELECT indisunique FROM pg_index
WHERE indexrelid = 'media_asset_copies_active_asset_store_idx'::regclass`).Scan(&unique); err != nil {
		t.Fatalf("reading restored copy index: %v", err)
	}
	if unique {
		t.Fatal("downgrade left the replacement index unique; version 39 requires the original non-unique index")
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("restoring latest copy index: %v", err)
	}
}

func TestMediaAssetCopyGenerationMigrationBackfillsAndRestoresReaders(t *testing.T) {
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
	migrateDownTo(t, dbURL, 40)
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
		t.Fatalf("opening version 40 test database: %v", err)
	}
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	var assetID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, derived_generation)
VALUES ($1, 'encoded', $2, 'encoded/migration.g4.mp4', 1, 4)
RETURNING id`, recordingID, profile).Scan(&assetID); err != nil {
		pool.Close()
		t.Fatalf("creating version 40 encoded media asset: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
VALUES ($1, 'migration-test', 'encoded/migration.g4.mp4', 'active')`, assetID); err != nil {
		pool.Close()
		t.Fatalf("creating version 40 active copy: %v", err)
	}
	pool.Close()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying copy generation migration: %v", err)
	}
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening migrated test database: %v", err)
	}
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT generation FROM media_asset_copies WHERE media_asset_id = $1`, assetID).Scan(&generation); err != nil {
		pool.Close()
		t.Fatalf("reading migrated copy generation: %v", err)
	}
	if generation != 4 {
		pool.Close()
		t.Fatalf("migrated copy generation = %d, want logical generation 4", generation)
	}
	var activeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM active_media_asset_copies($1, NULL)`, assetID).Scan(&activeCount); err != nil {
		pool.Close()
		t.Fatalf("reading current copies after migration: %v", err)
	}
	if activeCount != 1 {
		pool.Close()
		t.Fatalf("current copies after migration = %d, want 1", activeCount)
	}
	pool.Close()

	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("reverting copy generation migration: %v", err)
	}
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening downgraded test database: %v", err)
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM active_media_asset_copies($1, NULL)`, assetID).Scan(&activeCount); err != nil {
		t.Fatalf("reading active copies after downgrade: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("active copies after downgrade = %d, want 1", activeCount)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("restoring latest schema after downgrade check: %v", err)
	}
}
