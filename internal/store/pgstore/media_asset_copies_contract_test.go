package pgstore

import (
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
		t.Fatal("RequireAdoptedStoreRows succeeded while blank-store rows remained")
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
	assertLogicalAndCopyState(t, pool, assetID, "deleting", "tokyo", "deleting")
	if err := media.MarkDeleted(ctx, assetID); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	assertLogicalAndCopyState(t, pool, assetID, "deleted", "tokyo", "deleted")
	var hasDeletedAt bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&hasDeletedAt); err != nil {
		t.Fatalf("reading deleted copy timestamp: %v", err)
	}
	if !hasDeletedAt {
		t.Fatal("deleted copy has no deleted_at timestamp")
	}
}

func TestMediaAssetDeleteRollsBackWhenCopyIsMissing(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy: %v", err)
	}
	if err := NewMedia(pool).MarkDeleting(ctx, assetID); err == nil {
		t.Fatal("MarkDeleting succeeded without a physical copy row")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_assets WHERE id = $1`, assetID).Scan(&state); err != nil {
		t.Fatalf("reading logical asset state: %v", err)
	}
	if state != "active" {
		t.Fatalf("logical asset state after failed transition = %q, want active", state)
	}
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

func assertLogicalAndCopyState(t *testing.T, pool *pgxpool.Pool, assetID int64, wantLogical, storeName, wantCopy string) {
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
