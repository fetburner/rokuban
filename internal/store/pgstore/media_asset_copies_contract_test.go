package pgstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestAdoptUnassignedStoreRows(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, "tokyo")
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = 'sites/default/legacy-updated.m2ts', size_bytes = 20 WHERE id = $1`, assetID); err != nil {
		t.Fatalf("simulating legacy path update before adoption: %v", err)
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
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = 'sites/default/advanced_cut.g5.mp4' WHERE id = $1`, derivedAssetID); err != nil {
		t.Fatalf("simulating old writer advancing derived path: %v", err)
	}

	legacy := NewMedia(pool)
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
		{"media_asset_copies", "store"},
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
		legacyWriterAssetID: "sites/default/legacy-inserted.m2ts",
		derivedAssetID:      "sites/default/advanced_cut.g5.mp4",
	} {
		var gotRelPath string
		if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&gotRelPath); err != nil {
			t.Fatalf("reading adopted legacy copy %d: %v", assetID, err)
		}
		if gotRelPath != wantRelPath {
			t.Errorf("adopted copy %d path = %q, want %q", assetID, gotRelPath, wantRelPath)
		}
	}
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, derivedAssetID).Scan(&generation); err != nil {
		t.Fatalf("reading adopted derived generation: %v", err)
	}
	if generation != 5 {
		t.Fatalf("adopted derived_generation = %d, want 5 from the legacy path", generation)
	}
}

func TestListStorageSyncWithoutConfiguredStoreIncludesNamedObservations(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	workerMedia := NewMedia(pool, "tokyo")
	if err := workerMedia.UpsertStorageSync(ctx, store.StorageObservationInput{
		Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75,
	}); err != nil {
		t.Fatalf("recording worker storage observation: %v", err)
	}

	apiMedia := NewMedia(pool)
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
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1 AND store = ''`, assetID); err != nil {
		t.Fatalf("assigning fixture copy store: %v", err)
	}
	media := NewMedia(pool, "tokyo")

	if err := media.MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	assertLogicalAndCopyState(t, pool, assetID, "deleting", "deleting")
	if err := media.MarkDeleted(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	assertLogicalAndCopyState(t, pool, assetID, "deleted", "deleted")
	var hasDeletedAt bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&hasDeletedAt); err != nil {
		t.Fatalf("reading deleted copy timestamp: %v", err)
	}
	if !hasDeletedAt {
		t.Fatal("deleted copy has no deleted_at timestamp")
	}
}

func TestMediaAssetDeleteRepairsCopyMissingFromLegacyWriter(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy: %v", err)
	}
	if err := NewMedia(pool, "tokyo").MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting repairs the legacy copy: %v", err)
	}
	assertLogicalAndCopyState(t, pool, assetID, "deleting", "deleting")
	var relPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&relPath); err != nil {
		t.Fatalf("reading repaired copy path: %v", err)
	}
	var logicalRelPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_assets WHERE id = $1`, assetID).Scan(&logicalRelPath); err != nil {
		t.Fatalf("reading legacy asset path: %v", err)
	}
	if relPath != logicalRelPath {
		t.Fatalf("repaired copy path = %q, want legacy path %q", relPath, logicalRelPath)
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
	media := NewMedia(pool, "tokyo")
	if err := media.MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}

	var relPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_assets WHERE id = $1`, assetID).Scan(&relPath); err != nil {
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
	assertLogicalAndCopyState(t, pool, assetID, "deleting", "deleting")
}

func TestDerivedMediaAssetReplacementsClearCatalogSHA256(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	generation := int64(0)
	digest := bytes.Repeat([]byte{0x5a}, 32)

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
	setDigest(encodedID)
	generation = 1
	if _, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID, Profile: &profile, RelPath: "encoded/h264.g1.mp4", SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("replacing encoded asset: %v", err)
	}
	assertDigestCleared(encodedID)

	generation = 0
	thumbnailID, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1.jpg", SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating thumbnail asset: %v", err)
	}
	setDigest(thumbnailID)
	generation = 1
	if _, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1.g1.jpg", SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("recreating thumbnail asset: %v", err)
	}
	assertDigestCleared(thumbnailID)
	setDigest(thumbnailID)
	generation = 2
	if err := q.UpdateThumbnailMediaAssetPath(ctx, sqlcgen.UpdateThumbnailMediaAssetPathParams{
		RelPath: "thumbnails/1.g2.jpg", SizeBytes: 12, DerivedGeneration: &generation, ID: thumbnailID,
	}); err != nil {
		t.Fatalf("replacing thumbnail path: %v", err)
	}
	assertDigestCleared(thumbnailID)

	generation = 0
	seekTilesID, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1_tiles.jpg", SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating seek tiles asset: %v", err)
	}
	setDigest(seekTilesID)
	generation = 1
	if _, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID, RelPath: "thumbnails/1_tiles.g1.jpg", SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("recreating seek tiles asset: %v", err)
	}
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
			t.Errorf("tokyo catalog asset for recording %d = %+v, want tokyo copy and SHA %s", recordingID, got, wantSHA)
		}
		if got := gotOsaka[recordingID]; got.store != "osaka" || got.relPath != fmt.Sprintf("osaka/%d.m2ts", recordingID) || got.sha256 != wantSHA {
			t.Errorf("osaka catalog asset for recording %d = %+v, want osaka copy and SHA %s", recordingID, got, wantSHA)
		}
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

func assertLogicalAndCopyState(t *testing.T, pool *pgxpool.Pool, assetID int64, wantLogical, wantCopy string) {
	t.Helper()
	var logicalState, copyState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_assets WHERE id = $1`, assetID).Scan(&logicalState); err != nil {
		t.Fatalf("reading logical asset state: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&copyState); err != nil {
		t.Fatalf("reading copy state: %v", err)
	}
	if logicalState != wantLogical || copyState != wantCopy {
		t.Fatalf("logical/copy state = %q/%q, want %q/%q", logicalState, copyState, wantLogical, wantCopy)
	}
}
