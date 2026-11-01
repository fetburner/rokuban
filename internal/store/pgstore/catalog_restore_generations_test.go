package pgstore

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogRestoreKeepsCurrentGenerationOnLogicalRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "thumbnail",
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

	var derivedGeneration int64
	if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, assetID).Scan(&derivedGeneration); err != nil {
		t.Fatalf("reading restored logical thumbnail: %v", err)
	}
	if derivedGeneration != 8 {
		t.Fatalf("restored logical thumbnail generation = %d, want 8", derivedGeneration)
	}

	staleAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "seek_tiles",
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
	if err := pool.QueryRow(ctx, `SELECT derived_generation, size_bytes, sha256 FROM media_assets WHERE id = $1`, staleAssetID).Scan(&derivedGeneration, &sizeBytes, &sha256Bytes); err != nil {
		t.Fatalf("reading seek tiles after stale catalog restore: %v", err)
	}
	if derivedGeneration != 8 {
		t.Fatalf("stale catalog changed logical seek tiles to generation %d, want 8", derivedGeneration)
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
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating logical original: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	oldCreatedAt := now.Add(-time.Hour)
	oldDeletedAt := now.Add(-time.Minute)
	// Simulate a tombstone moved to its reserved migration path. Old catalogs
	// still carry the original path and must match this row by asset/store/generation.
	oldCopyID := insertCatalogRestoreCopy(t, pool, assetID, catalogMigrationTombstonePath("1"), "deleted", oldCreatedAt, &oldDeletedAt)
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies
		WHERE media_asset_id = $1 AND store = $2 AND state = 'deleted'`, assetID, dbtest.StoreName).Scan(&deletedCopies); err != nil {
		t.Fatalf("counting historical tombstones: %v", err)
	}
	if deletedCopies != 1 {
		t.Fatalf("historical tombstones = %d, want one after matching the migrated row %d by identity", deletedCopies, oldCopyID)
	}
	var hasActiveCopy bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM active_media_asset_copies($1, NULL))", assetID).Scan(&hasActiveCopy); err != nil {
		t.Fatalf("reading current copies of the logical original: %v", err)
	}
	if !hasActiveCopy {
		t.Fatal("logical original has no current active copy after restore")
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
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating historical logical original: %v", err)
	}
	insertCatalogRestoreCopy(t, pool, oldAssetID, catalogMigrationTombstonePath("2"), "deleted", oldCreatedAt, &oldDeletedAt)

	currentRecordingID := dbtest.Recording(t, pool)
	currentAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: currentRecordingID,
		Kind:        "original",
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
	var oldAssetTombstones int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies
		WHERE media_asset_id = $1 AND store = $2 AND state = 'deleted'`, oldAssetID, dbtest.StoreName).Scan(&oldAssetTombstones); err != nil {
		t.Fatalf("counting migrated tombstones for old asset: %v", err)
	}
	if oldAssetTombstones != 1 {
		t.Fatalf("old asset tombstones = %d, want one after identity match", oldAssetTombstones)
	}
}

func TestCatalogRestoreDoesNotResurrectCopyDeletedAfterStat(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	path := "originals/deleted-after-stat.ts"
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "original",
		SizeBytes:   100,
	})
	if err != nil {
		t.Fatalf("creating logical original: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Second)
	insertCatalogRestoreCopy(t, pool, assetID, path, "deleted", now.Add(-time.Hour), &deletedAt)

	// The catalog snapshot is active because rescue statted the file before the
	// delete worker unlinked it and committed the tombstone. Call the same restore
	// path without taking the rel_path lock to exercise the #982 database race.
	snapshot := []catalogdoc.MediaAsset{{
		ID: assetID, RecordingID: recordingID, Kind: "original",
		RelPath: path, SizeBytes: 100, State: "active",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
	}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), snapshot, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring active snapshot over completed deletion: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog restore: %v", err)
	}

	var activeCopies, deletedCopies int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state <> 'deleted'),
		count(*) FILTER (WHERE state = 'deleted')
		FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, dbtest.StoreName).Scan(&activeCopies, &deletedCopies); err != nil {
		t.Fatalf("counting restored copies: %v", err)
	}
	if activeCopies != 0 || deletedCopies != 1 {
		t.Fatalf("restored copies = active %d, deleted %d; want active 0, deleted 1", activeCopies, deletedCopies)
	}
}

func TestCatalogRestoreDoesNotResurrectMovedTombstoneAtReusedPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := "originals/reused-before-catalog-restore.ts"
	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Minute)

	deletedRecordingID := dbtest.Recording(t, pool)
	deletedAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: deletedRecordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating deleted logical original: %v", err)
	}
	insertCatalogRestoreCopy(t, pool, deletedAssetID, catalogMigrationTombstonePath("moved"), "deleted", now.Add(-time.Hour), &deletedAt)

	currentRecordingID := dbtest.Recording(t, pool)
	currentAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: currentRecordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating current logical original: %v", err)
	}
	currentCopyID := insertCatalogRestoreCopy(t, pool, currentAssetID, path, "active", now, nil)

	// The migration moved the deleted copy's tombstone off this physical path,
	// which a newer copy now owns. An older active catalog row for that identity
	// must still be ignored rather than aborting the generation or resurrecting it.
	snapshot := []catalogdoc.MediaAsset{{
		ID: deletedAssetID, RecordingID: deletedRecordingID, Kind: "original",
		RelPath: path, SizeBytes: 100, State: "active",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
	}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), snapshot, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring active snapshot over moved tombstone: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog restore: %v", err)
	}

	var activeCopies, deletedCopies int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state <> 'deleted'),
		count(*) FILTER (WHERE state = 'deleted')
		FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, deletedAssetID, dbtest.StoreName).Scan(&activeCopies, &deletedCopies); err != nil {
		t.Fatalf("counting moved tombstones after restore: %v", err)
	}
	if activeCopies != 0 || deletedCopies != 1 {
		t.Fatalf("restored copies = active %d, deleted %d; want active 0, deleted 1", activeCopies, deletedCopies)
	}
	var currentState string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, currentCopyID).Scan(&currentState); err != nil {
		t.Fatalf("reading current path owner: %v", err)
	}
	if currentState != "active" {
		t.Fatalf("current path owner state = %q, want active", currentState)
	}
}

func TestCatalogRestoreKeepsConflictingTombstoneAndContinuesGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := "originals/reused-by-new-copy.ts"
	now := time.Now().UTC().Truncate(time.Microsecond)
	oldDeletedAt := now.Add(-time.Minute)

	currentRecordingID := dbtest.Recording(t, pool)
	currentAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: currentRecordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating current logical original: %v", err)
	}
	currentCopyID := insertCatalogRestoreCopy(t, pool, currentAssetID, path, "active", now, nil)

	historicalRecordingID := dbtest.Recording(t, pool)
	historicalAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: historicalRecordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating historical logical original: %v", err)
	}

	otherRecordingID := dbtest.Recording(t, pool)
	otherAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: otherRecordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating following logical original: %v", err)
	}

	assets := []catalogdoc.MediaAsset{
		{
			ID: currentAssetID, RecordingID: currentRecordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: historicalAssetID, RecordingID: historicalRecordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "deleted", DeletedAt: &oldDeletedAt,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: otherAssetID, RecordingID: otherRecordingID, Kind: "original",
			RelPath: "originals/following.ts", SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now, Store: dbtest.StoreName,
		},
	}

	// This represents an old catalog written before the path owner changed. No
	// file lock is held: the tombstone INSERT must not abort this generation when
	// the newer active copy already owns the original path.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil, dbtest.StoreName); err != nil {
		t.Fatalf("restoring catalog with a conflicting tombstone: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog restore: %v", err)
	}

	var currentState string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, currentCopyID).Scan(&currentState); err != nil {
		t.Fatalf("reading current path owner: %v", err)
	}
	if currentState != "active" {
		t.Fatalf("current path owner state = %q, want active", currentState)
	}
	var historicalTombstones int
	var restoredTombstonePath string
	if err := pool.QueryRow(ctx, `SELECT count(*), min(rel_path)
		FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2 AND state = 'deleted'`,
		historicalAssetID, dbtest.StoreName).Scan(&historicalTombstones, &restoredTombstonePath); err != nil {
		t.Fatalf("reading restored historical tombstone: %v", err)
	}
	if historicalTombstones != 1 {
		t.Fatalf("historical tombstones = %d, want one", historicalTombstones)
	}
	if restoredTombstonePath == path || len(restoredTombstonePath) <= 1024 || strings.Contains(restoredTombstonePath, "/") {
		t.Fatalf("restored tombstone path length = %d, has separator %t, want a single component longer than 1024 bytes",
			len(restoredTombstonePath), strings.Contains(restoredTombstonePath, "/"))
	}
	var followingCopyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`,
		otherAssetID, dbtest.StoreName).Scan(&followingCopyCount); err != nil {
		t.Fatalf("checking later asset in restore batch: %v", err)
	}
	if followingCopyCount != 1 {
		t.Fatalf("later asset copies = %d, want one after tombstone conflict", followingCopyCount)
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

func catalogMigrationTombstonePath(id string) string {
	return strings.Repeat("t", 1025) + "-migration-" + id
}
