package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

type catalogRestorePreflightTx struct{ pgx.Tx }

func (catalogRestorePreflightTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("database accessed before catalog validation")
}

func TestApplyDocumentRejectsMissingMediaAssetFieldsBeforeDatabaseAccess(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	generation := int64(1)
	tests := []struct {
		name       string
		asset      catalogdoc.MediaAsset
		missingKey string
	}{
		{
			name: "store", missingKey: "store",
			asset: catalogdoc.MediaAsset{
				ID: 1, Kind: "original", RelPath: "originals/missing-store.ts",
				SizeBytes: 100, State: "active",
			},
		},
		{
			name: "derivedGeneration", missingKey: "derivedGeneration",
			asset: catalogdoc.MediaAsset{
				ID: 1, Kind: "thumbnail", RelPath: "thumbnails/missing-derived.g1.jpg",
				SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now,
				Generation: &generation, Store: dbtest.StoreName,
			},
		},
		{
			name: "generation", missingKey: "generation",
			asset: catalogdoc.MediaAsset{
				ID: 1, Kind: "thumbnail", RelPath: "thumbnails/missing-copy.g1.jpg",
				SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now,
				DerivedGeneration: &generation, Store: dbtest.StoreName,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &catalogdoc.Document{MediaAssets: []catalogdoc.MediaAsset{tt.asset}}
			if _, err := applyDocument(context.Background(), catalogRestorePreflightTx{}, doc, store.RestoreOptions{}, nil); err == nil || !strings.Contains(err.Error(), tt.missingKey) {
				t.Fatalf("applyDocument error = %v, want missing %s before database access", err, tt.missingKey)
			}
		})
	}
}

func TestCatalogRestoreRejectsMissingMediaAssetFieldsBeforeWriting(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	generation := int64(1)
	tests := []struct {
		name       string
		asset      catalogdoc.MediaAsset
		missingKey string
	}{
		{
			name:       "store",
			missingKey: "store",
			asset: catalogdoc.MediaAsset{
				ID: 1, RecordingID: 42, Kind: "original", RelPath: "originals/a.ts",
				SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now,
			},
		},
		{
			name:       "derivedGeneration",
			missingKey: "derivedGeneration",
			asset: catalogdoc.MediaAsset{
				ID: 1, RecordingID: 42, Kind: "thumbnail", RelPath: "thumbnails/a.g1.jpg",
				SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now,
				Generation: &generation, Store: dbtest.StoreName,
			},
		},
		{
			name:       "generation",
			missingKey: "generation",
			asset: catalogdoc.MediaAsset{
				ID: 1, RecordingID: 42, Kind: "thumbnail", RelPath: "thumbnails/a.g1.jpg",
				SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now,
				DerivedGeneration: &generation, Store: dbtest.StoreName,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			doc := &catalogdoc.Document{
				Rules: []catalogdoc.Rule{{
					ID: 1, Name: "preflight", Description: "", Enabled: true, Priority: 1,
					KeepOriginal: "always", EncodeProfiles: []string{}, FilenameTemplate: "",
					Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
				}},
				MediaAssets: []catalogdoc.MediaAsset{tt.asset},
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("beginning catalog restore: %v", err)
			}
			t.Cleanup(func() { _ = tx.Rollback(ctx) })

			if _, err := applyDocument(ctx, tx, doc, store.RestoreOptions{}, []string{"default"}); err == nil || !strings.Contains(err.Error(), tt.missingKey) {
				t.Fatalf("applyDocument error = %v, want missing %s", err, tt.missingKey)
			}
			var rules, assets int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM rules), (SELECT count(*) FROM media_assets)`).Scan(&rules, &assets); err != nil {
				t.Fatalf("checking writes after rejected catalog: %v", err)
			}
			if rules != 0 || assets != 0 {
				t.Fatalf("rows after rejected catalog = rules %d, assets %d; want no writes", rules, assets)
			}
		})
	}
}

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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil); err != nil {
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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), staleSnapshot, &restoreResult, nil); err != nil {
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
	// Simulate a tombstone that an earlier restore moved to its synthetic path. The
	// catalog still carries the original path and must match this row by identity and path.
	oldCopyID := insertCatalogRestoreCopy(t, pool, assetID, catalogTombstoneRelPath(sqlcgen.UpsertMediaAssetCopyParams{MediaAssetID: assetID, Generation: 0, RelPath: path}), "deleted", oldCreatedAt, &oldDeletedAt)
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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil); err != nil {
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

	// Rescue statted the file before the delete worker unlinked it and committed
	// the tombstone. Restoring that stale active snapshot must leave the path
	// non-active after the file is gone.
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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), snapshot, &restoreResult, nil); err != nil {
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
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil); err != nil {
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

func TestCatalogRestoreRestoresTwoTombstonesOfOneIdentity(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, assetID := createCatalogRestoreOriginal(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Minute)
	first, second := "originals/identity-first.ts", "originals/identity-second.ts"

	// One logical original deleted at two paths (a re-registered copy at a renamed
	// path, then deleted again). Both tombstones are in the catalog.
	applyCatalogMediaAssets(t, pool, []catalogdoc.MediaAsset{
		{
			ID: assetID, RecordingID: recordingID, Kind: "original",
			RelPath: first, SizeBytes: 100, State: "deleted", DeletedAt: &deletedAt,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: assetID, RecordingID: recordingID, Kind: "original",
			RelPath: second, SizeBytes: 100, State: "deleted", DeletedAt: &deletedAt,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
		},
	})

	paths := tombstonePaths(t, pool, assetID)
	if !slices.Contains(paths, first) || !slices.Contains(paths, second) {
		t.Fatalf("tombstone paths = %q, want both %q and %q", paths, first, second)
	}
}

func TestCatalogRestoreTwoTombstonesOfOneIdentityGetDistinctSyntheticPaths(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID, assetID := createCatalogRestoreOriginal(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Minute)
	first, second := "originals/owned-first.ts", "originals/owned-second.ts"

	// Both original paths are owned by live copies of other logical originals.
	_, ownerOfFirst := createCatalogRestoreOriginal(t, pool)
	_, ownerOfSecond := createCatalogRestoreOriginal(t, pool)
	insertCatalogRestoreCopy(t, pool, ownerOfFirst, first, "active", now, nil)
	insertCatalogRestoreCopy(t, pool, ownerOfSecond, second, "active", now, nil)

	applyCatalogMediaAssets(t, pool, []catalogdoc.MediaAsset{
		{
			ID: assetID, RecordingID: recordingID, Kind: "original",
			RelPath: first, SizeBytes: 100, State: "deleted", DeletedAt: &deletedAt,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: assetID, RecordingID: recordingID, Kind: "original",
			RelPath: second, SizeBytes: 100, State: "deleted", DeletedAt: &deletedAt,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
		},
	})

	paths := tombstonePaths(t, pool, assetID)
	if len(paths) != 2 || paths[0] == paths[1] {
		t.Fatalf("tombstone paths = %q, want two distinct synthetic paths", paths)
	}
	for _, p := range paths {
		if p == first || p == second || len(p) <= 1024 || strings.Contains(p, "/") {
			t.Fatalf("tombstone path %q is not a reserved single-component synthetic path", p)
		}
	}
	for _, owner := range []int64{ownerOfFirst, ownerOfSecond} {
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2 AND state <> 'deleted'`, owner, dbtest.StoreName).Scan(&state); err != nil {
			t.Fatalf("reading live owner %d: %v", owner, err)
		}
		if state != "active" {
			t.Fatalf("live owner %d state = %q, want active", owner, state)
		}
	}
}

func TestCatalogRestoreRerunKeepsOneSyntheticTombstone(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := "originals/rerun-conflict.ts"
	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Minute)

	currentRecordingID, currentAssetID := createCatalogRestoreOriginal(t, pool)
	currentCopyID := insertCatalogRestoreCopy(t, pool, currentAssetID, path, "active", now, nil)
	historicalRecordingID, historicalAssetID := createCatalogRestoreOriginal(t, pool)

	assets := []catalogdoc.MediaAsset{
		{
			ID: currentAssetID, RecordingID: currentRecordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "active", CreatedAt: now, UpdatedAt: now, Store: dbtest.StoreName,
		},
		{
			ID: historicalAssetID, RecordingID: historicalRecordingID, Kind: "original",
			RelPath: path, SizeBytes: 100, State: "deleted", DeletedAt: &deletedAt,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
		},
	}
	// The same catalog restored twice: the second run must find the synthetic
	// tombstone the first run wrote, not add another one or fail.
	applyCatalogMediaAssets(t, pool, assets)
	applyCatalogMediaAssets(t, pool, assets)

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, currentCopyID).Scan(&state); err != nil {
		t.Fatalf("reading current path owner: %v", err)
	}
	if state != "active" {
		t.Fatalf("current path owner state = %q, want active", state)
	}
	if paths := tombstonePaths(t, pool, historicalAssetID); len(paths) != 1 {
		t.Fatalf("historical tombstones after two restores = %q, want exactly one", paths)
	}
}

func TestCatalogRestoreAcceptsSyntheticTombstonePath(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, assetID := createCatalogRestoreOriginal(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Minute)
	original := "originals/already-moved.ts"
	// A catalog exported after an earlier restore lists the tombstone at its synthetic path.
	synthetic := catalogTombstoneRelPath(sqlcgen.UpsertMediaAssetCopyParams{MediaAssetID: assetID, Generation: 0, RelPath: original})

	applyCatalogMediaAssets(t, pool, []catalogdoc.MediaAsset{{
		ID: assetID, RecordingID: recordingID, Kind: "original",
		RelPath: synthetic, SizeBytes: 100, State: "deleted", DeletedAt: &deletedAt,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Store: dbtest.StoreName,
	}})

	paths := tombstonePaths(t, pool, assetID)
	if len(paths) != 1 || paths[0] != synthetic {
		t.Fatalf("tombstone paths = %q, want the synthetic path %q kept as is", paths, synthetic)
	}
}

// createCatalogRestoreOriginal は録画と原本の論理行を 1 組作り、その ID を返す。
func createCatalogRestoreOriginal(t *testing.T, pool *pgxpool.Pool) (recordingID, assetID int64) {
	t.Helper()
	ctx := context.Background()
	recordingID = dbtest.Recording(t, pool)
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID, Kind: "original", SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("creating logical original: %v", err)
	}
	return recordingID, assetID
}

// applyCatalogMediaAssets は catalog の media_assets を 1 tx で復元し、コミットする。
func applyCatalogMediaAssets(t *testing.T, pool *pgxpool.Pool, assets []catalogdoc.MediaAsset) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning catalog restore: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var restoreResult store.RestoreResult
	if err := applyMediaAssets(ctx, tx, sqlcgen.New(tx), assets, &restoreResult, nil); err != nil {
		t.Fatalf("restoring catalog media assets: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing catalog restore: %v", err)
	}
}

// tombstonePaths は asset の墓石の rel_path を昇順で返す。
func tombstonePaths(t *testing.T, pool *pgxpool.Pool, assetID int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT rel_path FROM media_asset_copies
		WHERE media_asset_id = $1 AND store = $2 AND state = 'deleted' ORDER BY rel_path`, assetID, dbtest.StoreName)
	if err != nil {
		t.Fatalf("querying tombstones of asset %d: %v", assetID, err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scanning tombstone of asset %d: %v", assetID, err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading tombstones of asset %d: %v", assetID, err)
	}
	return paths
}
