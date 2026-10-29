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

	// The unchanged observation tables retain their legacy conflict targets.
	// missing_media_assets no longer does: issue #1652 keys it by physical copy.
	legacyOrphanPath := fmt.Sprintf("legacy-writer-%d.m2ts", assets[0].id)
	for range 2 {
		if _, err := pool.Exec(ctx, `INSERT INTO orphan_files (rel_path) VALUES ($1) ON CONFLICT (rel_path) DO NOTHING`, legacyOrphanPath); err != nil {
			t.Fatalf("legacy orphan upsert with old conflict target: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes, observed_at)
VALUES ('media', '/legacy', 100, 25, 75, now())
ON CONFLICT (root) DO UPDATE SET path = EXCLUDED.path`); err != nil {
			t.Fatalf("legacy storage sync upsert with old conflict target: %v", err)
		}
	}
	var legacyOrphanCount, legacyStorageCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orphan_files WHERE rel_path = $1`, legacyOrphanPath).Scan(&legacyOrphanCount); err != nil {
		t.Fatalf("counting old-key orphan row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM storage_sync WHERE root = 'media'`).Scan(&legacyStorageCount); err != nil {
		t.Fatalf("counting old-key storage row: %v", err)
	}
	if legacyOrphanCount != 1 || legacyStorageCount != 1 {
		t.Fatalf("legacy conflict target rows = orphan %d / storage %d, want 1 / 1", legacyOrphanCount, legacyStorageCount)
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
