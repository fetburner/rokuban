package pgstore

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	currentSHA := bytes.Repeat([]byte{0x88}, 32)
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 8, sha256 = $2 WHERE id = $1`, staleAssetID, currentSHA); err != nil {
		t.Fatalf("setting current seek tiles generation: %v", err)
	}
	staleSHAHex := "7777777777777777777777777777777777777777777777777777777777777777"
	staleSnapshot := []catalogdoc.MediaAsset{{
		ID: staleAssetID, RecordingID: recordingID, Kind: "seek_tiles",
		RelPath: "thumbnails/old_tiles.g7.jpg", SizeBytes: 80, State: "active",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		DerivedGeneration: &oldGeneration, Generation: &oldGeneration, SHA256: &staleSHAHex,
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
	var sizeBytes int64
	var sha256Bytes []byte
	if err := pool.QueryRow(ctx, `SELECT rel_path, derived_generation, size_bytes, sha256 FROM media_assets WHERE id = $1`, staleAssetID).Scan(&relPath, &derivedGeneration, &sizeBytes, &sha256Bytes); err != nil {
		t.Fatalf("reading seek tiles after stale catalog restore: %v", err)
	}
	if relPath != "thumbnails/current_tiles.g8.jpg" || derivedGeneration != 8 {
		t.Fatalf("stale catalog changed logical seek tiles to (%q, generation %d), want current path and generation 8", relPath, derivedGeneration)
	}
	if sizeBytes != 100 || !bytes.Equal(sha256Bytes, currentSHA) {
		t.Fatalf("stale catalog changed current-generation size/hash to %d/%x, want 100/%x", sizeBytes, sha256Bytes, currentSHA)
	}
}

func TestCatalogRestoreHistoricalTombstoneKeepsRepublishedCopyActive(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	path := "originals/reused.ts"
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "original",
		RelPath:     path,
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating logical original: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	oldCreatedAt := now.Add(-time.Hour)
	oldDeletedAt := now.Add(-time.Minute)
	oldCopyID := insertCatalogRestoreCopy(t, pool, assetID, path, "deleted", oldCreatedAt, &oldDeletedAt)
	currentCopyID := insertCatalogRestoreCopy(t, pool, assetID, path, "active", now, nil)

	assets := []catalogdoc.MediaAsset{
		{
			ID: assetID, RecordingID: recordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "active",
			CreatedAt: now, UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: assetID, RecordingID: recordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "deleted", DeletedAt: &oldDeletedAt,
			CreatedAt: oldCreatedAt, UpdatedAt: now, Store: dbtest.StoreName,
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring active copy and its historical tombstone: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog restore: %v", err)
	}

	var activeState string
	if err := pool.QueryRow(ctx, "SELECT state FROM media_asset_copies WHERE id = $1", currentCopyID).Scan(&activeState); err != nil {
		t.Fatalf("reading republished copy: %v", err)
	}
	if activeState != "active" {
		t.Fatalf("republished copy state = %q, want active (historical copy %d is deleted)", activeState, oldCopyID)
	}
	var deletedCopies int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2 AND rel_path = $3 AND state = 'deleted'", assetID, dbtest.StoreName, path).Scan(&deletedCopies); err != nil {
		t.Fatalf("counting historical tombstones: %v", err)
	}
	if deletedCopies != 1 {
		t.Fatalf("historical tombstones = %d, want exactly one", deletedCopies)
	}
	var logicalState string
	if err := pool.QueryRow(ctx, "SELECT state FROM media_assets WHERE id = $1", assetID).Scan(&logicalState); err != nil {
		t.Fatalf("reading logical original state: %v", err)
	}
	if logicalState != "active" {
		t.Fatalf("logical original state = %q, want active", logicalState)
	}
}

func TestCatalogRestoreHistoricalTombstoneAllowsPathReuseByAnotherAsset(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := "originals/reused-across-assets.ts"
	now := time.Now().UTC().Truncate(time.Microsecond)
	oldCreatedAt := now.Add(-time.Hour)
	oldDeletedAt := now.Add(-time.Minute)

	oldRecordingID := dbtest.Recording(t, pool)
	oldAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: oldRecordingID,
		Kind:        "original",
		RelPath:     path,
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating historical logical original: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE media_assets SET state = 'deleted', deleted_at = $2 WHERE id = $1", oldAssetID, oldDeletedAt); err != nil {
		t.Fatalf("marking historical logical original deleted: %v", err)
	}
	insertCatalogRestoreCopy(t, pool, oldAssetID, path, "deleted", oldCreatedAt, &oldDeletedAt)

	currentRecordingID := dbtest.Recording(t, pool)
	currentAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: currentRecordingID,
		Kind:        "original",
		RelPath:     path,
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating current logical original: %v", err)
	}
	currentCopyID := insertCatalogRestoreCopy(t, pool, currentAssetID, path, "active", now, nil)

	assets := []catalogdoc.MediaAsset{
		{
			ID: currentAssetID, RecordingID: currentRecordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "active",
			CreatedAt: now, UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: oldAssetID, RecordingID: oldRecordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "deleted", DeletedAt: &oldDeletedAt,
			CreatedAt: oldCreatedAt, UpdatedAt: now, Store: dbtest.StoreName,
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring reused path with another asset's tombstone: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog restore: %v", err)
	}

	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM media_asset_copies WHERE id = $1", currentCopyID).Scan(&state); err != nil {
		t.Fatalf("reading current path owner: %v", err)
	}
	if state != "active" {
		t.Fatalf("current path owner state = %q, want active", state)
	}
}

func insertCatalogRestoreCopy(t *testing.T, pool *pgxpool.Pool, assetID int64, relPath, state string, createdAt time.Time, deletedAt *time.Time) int64 {
	t.Helper()
	rows, err := sqlcgen.New(pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID,
		Store:        dbtest.StoreName,
		RelPath:      relPath,
		Generation:   0,
		State:        state,
		CreatedAt:    &createdAt,
		DeletedAt:    deletedAt,
	})
	if err != nil {
		t.Fatalf("inserting %s copy for asset %d: %v", state, assetID, err)
	}
	if rows != 1 {
		t.Fatalf("inserting %s copy for asset %d affected %d rows, want one", state, assetID, rows)
	}
	var copyID int64
	if err := pool.QueryRow(context.Background(), "SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2 AND rel_path = $3 AND state = $4", assetID, dbtest.StoreName, relPath, state).Scan(&copyID); err != nil {
		t.Fatalf("reading %s copy for asset %d: %v", state, assetID, err)
	}
	return copyID
}
