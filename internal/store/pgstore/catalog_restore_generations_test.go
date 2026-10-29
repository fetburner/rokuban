package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogRestoreKeepsLogicalPathOnCurrentGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "thumbnail",
		RelPath:     "thumbnails/current.g8.jpg",
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating logical thumbnail: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 8 WHERE id = $1`, assetID); err != nil {
		t.Fatalf("setting current thumbnail generation: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	currentGeneration, oldGeneration := int64(8), int64(7)
	assets := []catalogdoc.MediaAsset{
		{
			ID: assetID, RecordingID: recordingID, Kind: "thumbnail",
			RelPath: "thumbnails/current.g8.jpg", SizeBytes: 100, State: "active",
			CreatedAt: now, UpdatedAt: now,
			DerivedGeneration: &currentGeneration, Generation: &currentGeneration,
			Store: dbtest.StoreName,
		},
		{
			ID: assetID, RecordingID: recordingID, Kind: "thumbnail",
			RelPath: "thumbnails/old.g7.jpg", SizeBytes: 100, State: "active",
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
			DerivedGeneration: &currentGeneration, Generation: &oldGeneration,
			Store: dbtest.StoreName,
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog media asset restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring current and old generation copies: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog media asset restore: %v", err)
	}

	var relPath string
	var derivedGeneration int64
	if err := pool.QueryRow(ctx, `
SELECT rel_path, derived_generation
FROM media_assets
WHERE id = $1`, assetID).Scan(&relPath, &derivedGeneration); err != nil {
		t.Fatalf("reading restored logical thumbnail: %v", err)
	}
	if relPath != "thumbnails/current.g8.jpg" || derivedGeneration != 8 {
		t.Fatalf("restored logical thumbnail = (%q, generation %d), want current path and generation 8", relPath, derivedGeneration)
	}

	staleAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "seek_tiles",
		RelPath:     "thumbnails/current_tiles.g8.jpg",
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating logical seek tiles: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 8 WHERE id = $1`, staleAssetID); err != nil {
		t.Fatalf("setting current seek tiles generation: %v", err)
	}
	staleSnapshot := []catalogdoc.MediaAsset{{
		ID: staleAssetID, RecordingID: recordingID, Kind: "seek_tiles",
		RelPath: "thumbnails/old_tiles.g7.jpg", SizeBytes: 80, State: "active",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		DerivedGeneration: &oldGeneration, Generation: &oldGeneration,
		Store: dbtest.StoreName,
	}}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning stale catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), staleSnapshot, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring an older catalog generation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing stale catalog restore: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT rel_path, derived_generation FROM media_assets WHERE id = $1`, staleAssetID).Scan(&relPath, &derivedGeneration); err != nil {
		t.Fatalf("reading seek tiles after stale catalog restore: %v", err)
	}
	if relPath != "thumbnails/current_tiles.g8.jpg" || derivedGeneration != 8 {
		t.Fatalf("stale catalog changed logical seek tiles to (%q, generation %d), want current path and generation 8", relPath, derivedGeneration)
	}
}
