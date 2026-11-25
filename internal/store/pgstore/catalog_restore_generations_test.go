package pgstore

import (
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

// 下の 2 件は、同じ論理資産の別の資産が合成パスに墓石を持つという状態を要する。
// その状態は Restore の経路では作れず（生きた原パスの所有者は墓石にされるまで原パスを離さない）、
// 生の SQL で作る。主張の結果（生きた所有者・墓石の件数）は Export で観測できるが、前提の状態が
// 方言に依る機構（墓石の合成パスへの移動）そのものなので、pgstore に残す。

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
	insertCatalogRestoreCopy(t, pool, oldAssetID, catalogTombstoneRelPath(sqlcgen.UpsertMediaAssetCopyParams{MediaAssetID: oldAssetID, Generation: 0, RelPath: path}), "deleted", oldCreatedAt, &oldDeletedAt)

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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil); err != nil {
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
	insertCatalogRestoreCopy(t, pool, deletedAssetID, catalogTombstoneRelPath(sqlcgen.UpsertMediaAssetCopyParams{MediaAssetID: deletedAssetID, Generation: 0, RelPath: path}), "deleted", now.Add(-time.Hour), &deletedAt)

	currentRecordingID := dbtest.Recording(t, pool)
	currentAssetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: currentRecordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating current logical original: %v", err)
	}
	currentCopyID := insertCatalogRestoreCopy(t, pool, currentAssetID, path, "active", now, nil)

	// An earlier restore moved the deleted copy's tombstone off this physical path,
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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), snapshot, &restoreResult, nil); err != nil {
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
