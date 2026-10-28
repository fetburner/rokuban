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

func TestMediaAssetCopyCompatibilityShapeIsRetired(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var compatibilityViewExists, storeFunctionExists bool
	if err := pool.QueryRow(ctx, `
SELECT to_regclass('public.until_encoded_deletable_originals') IS NOT NULL,
       to_regprocedure('public.until_encoded_deletable_originals(text)') IS NOT NULL`).
		Scan(&compatibilityViewExists, &storeFunctionExists); err != nil {
		t.Fatalf("checking until-encoded compatibility objects: %v", err)
	}
	if compatibilityViewExists || !storeFunctionExists {
		t.Fatalf("compatibility view/function exists = %v/%v, want false/true", compatibilityViewExists, storeFunctionExists)
	}

	rows, err := pool.Query(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'removed_encoded_assets'
ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("reading removed_encoded_assets shape: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scanning removed_encoded_assets column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading removed_encoded_assets columns: %v", err)
	}
	if fmt.Sprint(columns) != "[asset_id recording_id]" {
		t.Fatalf("removed_encoded_assets columns = %v, want [asset_id recording_id]", columns)
	}
}

func TestAdoptRejectsCopylessAssets(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, "tokyo", "")
	unassignedMedia := NewMedia(pool, "", "")
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/unassigned.m2ts"
	})
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy to simulate an unassigned asset: %v", err)
	}

	if err := unassignedMedia.UpsertIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 10, TempRelPath: "sites/default/.rokuban-ingest-test", TempGeneration: 1,
	}); err != nil {
		t.Fatalf("seeding ingest progress: %v", err)
	}
	if err := unassignedMedia.UpsertOrphanFile(ctx, "sites/default/orphan.m2ts"); err != nil {
		t.Fatalf("seeding orphan observation: %v", err)
	}
	if err := unassignedMedia.UpsertMissingMediaAsset(ctx, assetID); err != nil {
		t.Fatalf("seeding missing asset observation: %v", err)
	}
	if err := unassignedMedia.UpsertStorageSync(ctx, store.StorageObservationInput{
		Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75,
	}); err != nil {
		t.Fatalf("seeding storage observation: %v", err)
	}
	if err := media.RequireMediaAssetCopiesAssigned(ctx); err == nil {
		t.Fatal("RequireMediaAssetCopiesAssigned succeeded while a copyless asset remained")
	}
	if err := media.AdoptUnassignedStoreRows(ctx); err == nil {
		t.Fatal("AdoptUnassignedStoreRows succeeded while a copyless asset remained")
	}
	var unassignedIngest int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_ingest_progress WHERE temp_store = ''`).Scan(&unassignedIngest); err != nil {
		t.Fatalf("checking that adopt did not partially update ingest progress: %v", err)
	}
	if unassignedIngest != 1 {
		t.Fatalf("unassigned ingest progress after rejected adopt = %d, want 1", unassignedIngest)
	}

	if err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: dbtest.StoreName, RelPath: "sites/default/unassigned.m2ts", State: "active",
	}); err != nil {
		t.Fatalf("restoring fixture copy: %v", err)
	}
	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows after copies are assigned: %v", err)
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
	assertCopyState(t, pool, assetID, "tokyo", "deleting")
	if err := media.MarkDeleted(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	assertCopyState(t, pool, assetID, "tokyo", "deleted")
	var hasDeletedAt bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&hasDeletedAt); err != nil {
		t.Fatalf("reading deleted copy timestamp: %v", err)
	}
	if !hasDeletedAt {
		t.Fatal("deleted copy has no deleted_at timestamp")
	}
}

func TestMediaAssetDeleteDoesNotRequireMissingCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")
	if err := media.MarkDeleting(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleting with a missing physical copy: %v", err)
	}
	var copies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, assetID).Scan(&copies); err != nil {
		t.Fatalf("counting physical copies: %v", err)
	}
	if copies != 0 {
		t.Fatalf("physical copy count = %d, want 0", copies)
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
	assertCopyState(t, pool, assetID, dbtest.StoreName, "deleting")
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
		RecordingID: recordingID, Profile: &profile, SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating encoded asset: %v", err)
	}
	setCopy(encodedID, "encoded/h264.mp4")
	setDigest(encodedID)
	generation = 1
	if _, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID, Profile: &profile, SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("replacing encoded asset: %v", err)
	}
	setCopy(encodedID, "encoded/h264.g1.mp4")
	assertDigestCleared(encodedID)

	generation = 0
	thumbnailID, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID, SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating thumbnail asset: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.jpg")
	setDigest(thumbnailID)
	generation = 1
	if _, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
		RecordingID: recordingID, SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("recreating thumbnail asset: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.g1.jpg")
	assertDigestCleared(thumbnailID)
	setDigest(thumbnailID)
	generation = 2
	if err := q.UpdateThumbnailMediaAssetPath(ctx, sqlcgen.UpdateThumbnailMediaAssetPathParams{
		SizeBytes: 12, DerivedGeneration: &generation,
		ID: thumbnailID, StoreName: dbtest.StoreName,
	}); err != nil {
		t.Fatalf("replacing thumbnail path: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.g2.jpg")
	assertDigestCleared(thumbnailID)

	generation = 0
	seekTilesID, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID, SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating seek tiles asset: %v", err)
	}
	setCopy(seekTilesID, "thumbnails/1_tiles.jpg")
	setDigest(seekTilesID)
	generation = 1
	replacedSeekTilesID, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID, SizeBytes: 11, DerivedGeneration: &generation,
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

func assertCopyState(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName, want string) {
	t.Helper()
	var copyState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, storeName).Scan(&copyState); err != nil {
		t.Fatalf("reading copy state: %v", err)
	}
	if copyState != want {
		t.Fatalf("copy state = %q, want %q", copyState, want)
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
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES ($1, 'encoded', $2, 1, $3)`, recordingID, profile, generation); err != nil {
			t.Fatalf("seeding encoded %s: %v", profile, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'thumbnail', 1, 4)`, recordingID); err != nil {
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
