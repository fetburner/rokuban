package pgstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestMediaAssetCopyReaderMigrationKeepsLegacyRelations(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	queries := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "legacy until-encoded relation",
			sql: `SELECT count(*) FROM (
    SELECT asset_id, recording_id, rel_path, size_bytes, state
    FROM until_encoded_deletable_originals
    LIMIT 0
) legacy_rows`,
		},
		{
			name: "store-aware until-encoded function",
			sql:  `SELECT count(*) FROM until_encoded_deletable_originals($1)`,
			args: []any{dbtest.StoreName},
		},
		{
			name: "legacy removed-encoded relation",
			sql: `SELECT count(*) FROM (
    SELECT asset_id, recording_id, rel_path, size_bytes, state
    FROM removed_encoded_assets
    LIMIT 0
) legacy_rows`,
		},
	}
	for _, query := range queries {
		t.Run(query.name, func(t *testing.T) {
			var count int64
			if err := pool.QueryRow(ctx, query.sql, query.args...).Scan(&count); err != nil {
				t.Fatalf("legacy reader query failed: %v", err)
			}
			if count != 0 {
				t.Fatalf("empty fixture query returned %d rows, want 0", count)
			}
		})
	}
}

func TestAdoptUnassignedStoreRows(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, "tokyo", "")
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	// マイグレーション直後の論理資産はコピー行を持たない（未割り当て）。
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy to simulate an unassigned asset: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = 'sites/default/legacy-updated.m2ts', size_bytes = 20 WHERE id = $1`, assetID); err != nil {
		t.Fatalf("simulating legacy path update before adoption: %v", err)
	}
	// 指定置き場の既存コピーは正とし、古い列がずれても adopt で上書きしない。
	syncedRecording := dbtest.Recording(t, pool)
	syncedAssetID := dbtest.MediaAsset(t, pool, syncedRecording)
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1`, syncedAssetID); err != nil {
		t.Fatalf("assigning fixture copy to tokyo: %v", err)
	}
	var syncedCopyRelPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, syncedAssetID).Scan(&syncedCopyRelPath); err != nil {
		t.Fatalf("reading initial tokyo copy path: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = 'sites/default/stale-logical-path.m2ts', state = 'deleted', deleted_at = now() WHERE id = $1`, syncedAssetID); err != nil {
		t.Fatalf("simulating legacy path update on an assigned asset: %v", err)
	}
	// 別の置き場だけにコピーがある資産には adopt は触れない。
	otherRecording := dbtest.Recording(t, pool)
	otherAssetID := dbtest.MediaAsset(t, pool, otherRecording)
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'osaka' WHERE media_asset_id = $1`, otherAssetID); err != nil {
		t.Fatalf("assigning fixture copy to osaka: %v", err)
	}
	legacyWriterRecording := dbtest.Recording(t, pool)
	var legacyWriterAssetID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'original', 'sites/default/legacy-inserted.m2ts', 30)
RETURNING id`, legacyWriterRecording).Scan(&legacyWriterAssetID); err != nil {
		t.Fatalf("simulating legacy writer insert without a copy: %v", err)
	}
	derivedRecordingID := dbtest.Recording(t, pool)
	var derivedAssetID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state, derived_generation)
VALUES ($1, 'encoded', 'cut', 'sites/default/advanced_cut.g4.mp4', 40, 'active', 4)
RETURNING id`, derivedRecordingID).Scan(&derivedAssetID); err != nil {
		t.Fatalf("seeding migrated derived asset: %v", err)
	}
	// The physical copy is at generation 5 while the compatibility column still says 4.
	if err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: derivedAssetID,
		Store:        "tokyo",
		RelPath:      "sites/default/advanced_cut.g5.mp4",
		State:        "active",
	}); err != nil {
		t.Fatalf("seeding advanced derived copy path: %v", err)
	}

	legacy := NewMedia(pool, "tokyo", "")
	if err := legacy.UpsertIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 10, TempRelPath: "sites/default/.rokuban-ingest-test", TempGeneration: 1,
	}); err != nil {
		t.Fatalf("seeding ingest progress: %v", err)
	}
	if err := legacy.UpsertOrphanFile(ctx, "sites/default/orphan.m2ts"); err != nil {
		t.Fatalf("seeding orphan observation: %v", err)
	}
	if err := legacy.UpsertMissingMediaAsset(ctx, assetID); err != nil {
		t.Fatalf("seeding missing asset observation: %v", err)
	}
	if err := legacy.UpsertStorageSync(ctx, store.StorageObservationInput{
		Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75,
	}); err != nil {
		t.Fatalf("seeding storage observation: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err == nil {
		t.Fatal("RequireAdoptedStoreRows succeeded while blank-store or copyless rows remained")
	}

	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err != nil {
		t.Fatalf("RequireAdoptedStoreRows after adoption: %v", err)
	}

	for _, tableAndColumn := range [][2]string{
		{"recording_ingest_progress", "temp_store"},
		{"orphan_files", "store"},
		{"missing_media_assets", "store"},
		{"storage_sync", "store"},
	} {
		table, column := tableAndColumn[0], tableAndColumn[1]
		query := fmt.Sprintf(`SELECT count(*) FILTER (WHERE %s = ''), count(*) FILTER (WHERE %s = 'tokyo') FROM %s`, column, column, table)
		var unassigned, adopted int
		if err := pool.QueryRow(ctx, query).Scan(&unassigned, &adopted); err != nil {
			t.Fatalf("counting stores in %s: %v", table, err)
		}
		if unassigned != 0 || adopted == 0 {
			t.Errorf("%s.%s counts = unassigned %d / adopted %d, want 0 / positive", table, column, unassigned, adopted)
		}
	}

	// Re-running adopt leaves already assigned rows in their current store.
	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("re-running AdoptUnassignedStoreRows: %v", err)
	}
	var copyStore string
	if err := pool.QueryRow(ctx, `SELECT store FROM media_asset_copies WHERE media_asset_id = $1`, assetID).Scan(&copyStore); err != nil {
		t.Fatalf("reading adopted copy store: %v", err)
	}
	if copyStore != "tokyo" {
		t.Fatalf("adopted copy store = %q, want tokyo", copyStore)
	}
	for assetID, wantRelPath := range map[int64]string{
		assetID:             "sites/default/legacy-updated.m2ts",
		syncedAssetID:       syncedCopyRelPath,
		legacyWriterAssetID: "sites/default/legacy-inserted.m2ts",
		derivedAssetID:      "sites/default/advanced_cut.g5.mp4",
	} {
		var gotRelPath, gotState string
		if err := pool.QueryRow(ctx, `SELECT rel_path, state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&gotRelPath, &gotState); err != nil {
			t.Fatalf("reading adopted legacy copy %d: %v", assetID, err)
		}
		if gotRelPath != wantRelPath {
			t.Errorf("adopted copy %d path = %q, want %q", assetID, gotRelPath, wantRelPath)
		}
		if gotState != "active" {
			t.Errorf("adopted copy %d state = %q, want active", assetID, gotState)
		}
	}
	var otherCopies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'osaka'`, otherAssetID).Scan(&otherCopies); err != nil || otherCopies != 1 {
		t.Fatalf("osaka-only copy count = %d (err %v), want 1 untouched", otherCopies, err)
	}
	var tokyoOfOther int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, otherAssetID).Scan(&tokyoOfOther); err != nil || tokyoOfOther != 0 {
		t.Fatalf("tokyo copies of an osaka-only asset = %d (err %v), want 0", tokyoOfOther, err)
	}
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, derivedAssetID).Scan(&generation); err != nil {
		t.Fatalf("reading adopted derived generation: %v", err)
	}
	if generation != 5 {
		t.Fatalf("adopted derived_generation = %d, want 5 from the adopted copy path", generation)
	}
}

func TestListStorageSyncWithoutConfiguredStoreIncludesNamedObservations(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	workerMedia := NewMedia(pool, "tokyo", "")
	if err := workerMedia.UpsertStorageSync(ctx, store.StorageObservationInput{
		Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75,
	}); err != nil {
		t.Fatalf("recording worker storage observation: %v", err)
	}

	apiMedia := NewMedia(pool, "tokyo", "")
	rows, err := apiMedia.ListStorageSync(ctx)
	if err != nil {
		t.Fatalf("listing storage observations without configured store: %v", err)
	}
	if len(rows) != 1 || rows[0].Root != "media" || rows[0].Path != "/media" {
		t.Fatalf("API-only storage observations = %+v, want named worker observation", rows)
	}
}

func TestMediaAssetCopyTracksDeleteLifecycle(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("assigning fixture copy store: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")

	if err := media.MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	assertLogicalAndCopyState(t, pool, assetID, "tokyo", "deleting", "deleting")
	if err := media.MarkDeleted(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	assertLogicalAndCopyState(t, pool, assetID, "tokyo", "deleted", "deleted")
	var hasDeletedAt bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&hasDeletedAt); err != nil {
		t.Fatalf("reading deleted copy timestamp: %v", err)
	}
	if !hasDeletedAt {
		t.Fatal("deleted copy has no deleted_at timestamp")
	}
}

func TestMediaAssetDeleteDoesNotReadLegacyColumnsWhenCopyIsMissing(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")
	if err := media.RequireAdoptedStoreRows(ctx); err == nil {
		t.Fatal("RequireAdoptedStoreRows succeeded while the physical copy was missing")
	}
	if err := media.MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting with a missing physical copy: %v", err)
	}
	var logicalState string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_assets WHERE id = $1`, assetID).Scan(&logicalState); err != nil {
		t.Fatalf("reading unchanged logical state: %v", err)
	}
	var copies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, assetID).Scan(&copies); err != nil {
		t.Fatalf("counting physical copies: %v", err)
	}
	if logicalState != "active" || copies != 0 {
		t.Fatalf("logical state/copy count = %q/%d, want active/0 until adoption", logicalState, copies)
	}
}

func TestMediaAssetCopiesAcceptLeadingHyphenStoreName(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: "-tokyo", RelPath: "tokyo/original.m2ts", State: "active",
	}); err != nil {
		t.Fatalf("storing accepted leading-hyphen name: %v", err)
	}
}

func TestRegisterInPlacePreservesDeletingCopyState(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	media := NewMedia(pool, dbtest.StoreName, "")
	if err := media.MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}

	var relPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, dbtest.StoreName).Scan(&relPath); err != nil {
		t.Fatalf("reading deleting asset path: %v", err)
	}
	result, err := media.RegisterInPlace(ctx, store.InPlaceInput{
		Assets: []store.InPlaceAsset{{Kind: store.AssetKindOriginal, RelPath: relPath, SizeBytes: 1}},
	})
	if err != nil {
		t.Fatalf("RegisterInPlace: %v", err)
	}
	if result.RecordingID != recordingID || len(result.AssetIDs) != 1 || result.AssetIDs[0] != assetID {
		t.Fatalf("RegisterInPlace result = %+v, want existing recording %d and asset %d", result, recordingID, assetID)
	}
	assertLogicalAndCopyState(t, pool, assetID, dbtest.StoreName, "deleting", "deleting")
}

func TestDerivedMediaAssetReplacementsClearCatalogSHA256(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	generation := int64(0)
	digest := bytes.Repeat([]byte{0x5a}, 32)
	setCopy := func(assetID int64, relPath string) {
		t.Helper()
		if err := q.UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
			MediaAssetID: assetID, Store: dbtest.StoreName, RelPath: relPath, State: "active",
		}); err != nil {
			t.Fatalf("seeding active copy for media asset %d: %v", assetID, err)
		}
	}

	setDigest := func(assetID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET sha256 = $2 WHERE id = $1`, assetID, digest); err != nil {
			t.Fatalf("seeding catalog SHA-256 for media asset %d: %v", assetID, err)
		}
	}
	assertDigestCleared := func(assetID int64) {
		t.Helper()
		var got []byte
		if err := pool.QueryRow(ctx, `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&got); err != nil {
			t.Fatalf("reading catalog SHA-256 for media asset %d: %v", assetID, err)
		}
		if got != nil {
			t.Fatalf("media asset %d retained catalog SHA-256 %x after content replacement", assetID, got)
		}
	}

	encodedID, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID, Profile: &profile, RelPath: "encoded/h264.mp4", SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating encoded asset: %v", err)
	}
	setCopy(encodedID, "encoded/h264.mp4")
	setDigest(encodedID)
	generation = 1
	if _, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID, Profile: &profile, RelPath: "encoded/h264.g1.mp4", SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("replacing encoded asset: %v", err)
	}
	setCopy(encodedID, "encoded/h264.g1.mp4")
	assertDigestCleared(encodedID)

	generation = 0
	thumbnailID, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1.jpg", SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating thumbnail asset: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.jpg")
	setDigest(thumbnailID)
	generation = 1
	if _, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1.g1.jpg", SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("recreating thumbnail asset: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.g1.jpg")
	assertDigestCleared(thumbnailID)
	setDigest(thumbnailID)
	generation = 2
	if err := q.UpdateThumbnailMediaAssetPath(ctx, sqlcgen.UpdateThumbnailMediaAssetPathParams{
		RelPath: "thumbnails/1.g2.jpg", SizeBytes: 12, DerivedGeneration: &generation,
		ID: thumbnailID, StoreName: dbtest.StoreName,
	}); err != nil {
		t.Fatalf("replacing thumbnail path: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.g2.jpg")
	assertDigestCleared(thumbnailID)

	generation = 0
	seekTilesID, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1_tiles.jpg", SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating seek tiles asset: %v", err)
	}
	setCopy(seekTilesID, "thumbnails/1_tiles.jpg")
	setDigest(seekTilesID)
	generation = 1
	replacedSeekTilesID, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1_tiles.g1.jpg", SizeBytes: 11, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("recreating seek tiles asset: %v", err)
	}
	if replacedSeekTilesID != seekTilesID {
		t.Fatalf("seek tiles replacement ID = %d, want existing asset %d", replacedSeekTilesID, seekTilesID)
	}
	setCopy(seekTilesID, "thumbnails/1_tiles.g1.jpg")
	assertDigestCleared(seekTilesID)
}

func TestCatalogExportSelectsStoreCopyAndKeepsFullInventory(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	assets := make(map[int64]int64)
	shaByRecording := make(map[int64]string)
	for i := range 2 {
		recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Title = fmt.Sprintf("catalog copy %d", i) })
		assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
			p.RelPath = fmt.Sprintf("legacy/%d.m2ts", recordingID)
		})
		assets[recordingID] = assetID
		if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
			t.Fatalf("removing default fixture copy: %v", err)
		}
		sum := make([]byte, 32)
		for n := range sum {
			sum[n] = byte(n + i)
		}
		shaByRecording[recordingID] = hex.EncodeToString(sum)
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET sha256 = $2 WHERE id = $1`, assetID, sum); err != nil {
			t.Fatalf("setting SHA-256: %v", err)
		}
		for _, storeName := range []string{"tokyo", "osaka"} {
			if err := q.UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
				MediaAssetID: assetID, Store: storeName, RelPath: fmt.Sprintf("%s/%d.m2ts", storeName, recordingID), State: "active",
			}); err != nil {
				t.Fatalf("adding %s copy: %v", storeName, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = $2, state = 'deleted', deleted_at = now() WHERE id = $1`, assetID, fmt.Sprintf("stale/%d.m2ts", recordingID)); err != nil {
			t.Fatalf("making logical legacy columns disagree with copies: %v", err)
		}
	}

	tokyo, err := NewCatalog(pool, []string{"default"}, "tokyo").Export(ctx)
	if err != nil {
		t.Fatalf("exporting tokyo catalog: %v", err)
	}
	osaka, err := NewCatalog(pool, []string{"default"}, "osaka").Export(ctx)
	if err != nil {
		t.Fatalf("exporting osaka catalog: %v", err)
	}
	if len(tokyo.Recordings) != 2 || len(osaka.Recordings) != len(tokyo.Recordings) {
		t.Fatalf("recording inventory tokyo/osaka = %d/%d, want full inventory 2/2", len(tokyo.Recordings), len(osaka.Recordings))
	}
	if len(tokyo.MediaAssets) != 2 || len(osaka.MediaAssets) != len(tokyo.MediaAssets) {
		t.Fatalf("asset inventory tokyo/osaka = %d/%d, want full inventory 2/2", len(tokyo.MediaAssets), len(osaka.MediaAssets))
	}
	byRecording := func(rows []catalogAssetCopy) map[int64]catalogAssetCopy {
		out := make(map[int64]catalogAssetCopy, len(rows))
		for _, row := range rows {
			out[row.recordingID] = row
		}
		return out
	}
	gotTokyo := byRecording(mediaAssetCopyRows(tokyo.MediaAssets))
	gotOsaka := byRecording(mediaAssetCopyRows(osaka.MediaAssets))
	for recordingID := range assets {
		wantSHA := shaByRecording[recordingID]
		if got := gotTokyo[recordingID]; got.store != "tokyo" || got.relPath != fmt.Sprintf("tokyo/%d.m2ts", recordingID) || got.sha256 != wantSHA {
			t.Errorf("tokyo catalog asset for recording %d = %+v, want tokyo copy path and logical SHA %s", recordingID, got, wantSHA)
		}
		if got := gotOsaka[recordingID]; got.store != "osaka" || got.relPath != fmt.Sprintf("osaka/%d.m2ts", recordingID) || got.sha256 != wantSHA {
			t.Errorf("osaka catalog asset for recording %d = %+v, want osaka copy path and logical SHA %s", recordingID, got, wantSHA)
		}
	}
}

func TestMediaServingRequiresAnActiveStoreCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "legacy/original.m2ts"
	})
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo', rel_path = 'copies/tokyo/original.m2ts' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("setting the physical copy path: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = 'stale/logical-path.m2ts', state = 'deleted', deleted_at = now() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("making legacy columns disagree with the active copy: %v", err)
	}

	media := NewMedia(pool, "tokyo", "")
	got, err := media.GetOriginalMediaAssetForServing(ctx, recordingID)
	if err != nil {
		t.Fatalf("serving active copy: %v", err)
	}
	if got.Store != "tokyo" || got.RelPath != "copies/tokyo/original.m2ts" {
		t.Fatalf("served asset = %+v, want the active tokyo copy", got)
	}

	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID); err != nil {
		t.Fatalf("deleting the physical copy: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = 'active', deleted_at = NULL WHERE id = $1`, assetID); err != nil {
		t.Fatalf("making legacy columns claim active: %v", err)
	}
	if _, err := media.GetOriginalMediaAssetForServing(ctx, recordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serving without an active copy error = %v, want ErrNotFound", err)
	}
	if _, err := media.GetActiveOriginalMediaAsset(ctx, recordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reading an available original without an active copy error = %v, want ErrNotFound", err)
	}
}

func TestTrashDeleteCandidatesUseActiveCopyPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "legacy/original.m2ts"
	})
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() - interval '2 days' WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("putting recording in trash: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo', rel_path = 'copies/tokyo/original.m2ts', state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("making physical copy deleted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = 'stale/logical-path.m2ts', state = 'active', deleted_at = NULL WHERE id = $1`, assetID); err != nil {
		t.Fatalf("making legacy columns disagree with the deleted copy: %v", err)
	}

	media := NewMedia(pool, "tokyo", "")
	rows, err := media.ListTrashAssetsToDelete(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("listing trash deletion candidates with a deleted copy: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("trash deletion candidates with a deleted copy = %+v, want none", rows)
	}

	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'active', deleted_at = NULL WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID); err != nil {
		t.Fatalf("activating physical copy: %v", err)
	}
	rows, err = media.ListTrashAssetsToDelete(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("listing trash deletion candidates with an active copy: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != assetID || rows[0].RelPath != "copies/tokyo/original.m2ts" {
		t.Fatalf("trash deletion candidates = %+v, want asset %d at its active copy path", rows, assetID)
	}
}

type catalogAssetCopy struct {
	recordingID int64
	store       string
	relPath     string
	sha256      string
}

func mediaAssetCopyRows(assets []catalogdoc.MediaAsset) []catalogAssetCopy {
	rows := make([]catalogAssetCopy, 0, len(assets))
	for _, asset := range assets {
		sha := ""
		if asset.SHA256 != nil {
			sha = *asset.SHA256
		}
		rows = append(rows, catalogAssetCopy{recordingID: asset.RecordingID, store: asset.Store, relPath: asset.RelPath, sha256: sha})
	}
	return rows
}

func assertLogicalAndCopyState(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName, wantLogical, wantCopy string) {
	t.Helper()
	var logicalState, copyState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_assets WHERE id = $1`, assetID).Scan(&logicalState); err != nil {
		t.Fatalf("reading logical asset state: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, storeName).Scan(&copyState); err != nil {
		t.Fatalf("reading copy state: %v", err)
	}
	if logicalState != wantLogical || copyState != wantCopy {
		t.Fatalf("logical/copy state = %q/%q, want %q/%q", logicalState, copyState, wantLogical, wantCopy)
	}
}

// profile を持つ kind（encoded は profile ごとに複数行）は単一の世代を持たない。
// :one が任意の行を返さず「無い」を返すことを固定する。
func TestGetDerivedMediaAssetGenerationIgnoresKindsWithProfiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	for profile, generation := range map[string]int64{"h264": 3, "h265": 7} {
		if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, derived_generation)
VALUES ($1, 'encoded', $2, $3, 1, $4)`, recordingID, profile, "sites/default/"+profile+".mp4", generation); err != nil {
			t.Fatalf("seeding encoded %s: %v", profile, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, derived_generation)
VALUES ($1, 'thumbnail', 'thumbnails/x.g4.jpg', 1, 4)`, recordingID); err != nil {
		t.Fatalf("seeding thumbnail: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")
	if got, found, err := media.GetDerivedMediaAssetGeneration(ctx, recordingID, "encoded"); err != nil || found {
		t.Fatalf("encoded generation = %d, found %v, err %v; want not found", got, found, err)
	}
	if got, found, err := media.GetDerivedMediaAssetGeneration(ctx, recordingID, "thumbnail"); err != nil || !found || got != 4 {
		t.Fatalf("thumbnail generation = %d, found %v, err %v; want 4", got, found, err)
	}
}
