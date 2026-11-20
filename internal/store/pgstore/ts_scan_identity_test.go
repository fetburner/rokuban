package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func scanOriginalInput(path string) store.InPlaceInput {
	input := inPlaceEncodedInput(path, 188)
	input.Assets[0].Kind = store.AssetKindOriginal
	input.Assets[0].Profile = nil
	return input
}

func requireCurrentScan(t *testing.T, pool *pgxpool.Pool, id int64, want bool) {
	t.Helper()
	var got bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM current_ts_scanned_originals WHERE media_asset_id = $1)`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("current scan = %v, want %v", got, want)
	}
}

func TestInPlaceOriginalInvalidatesTSScan(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	media := testMedia(pool, dbtest.StoreName, "")
	input := scanOriginalInput("original/first.ts")
	result, err := media.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	id := result.AssetIDs[0]
	requireCurrentScan(t, pool, id, false)
	source, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := media.ReplaceScan(ctx, source, 188, nil, nil); err != nil || !ok {
		t.Fatalf("initial scan = %v, %v", ok, err)
	}
	requireCurrentScan(t, pool, id, true)
	if _, err := media.RegisterInPlace(ctx, input); err != nil {
		t.Fatal(err)
	}
	requireCurrentScan(t, pool, id, true) // The same active path is a no-op.
	input.Assets[0].RelPath = "original/blocked.ts"
	if _, err := media.RegisterInPlace(ctx, input); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("registration with live copy = %v, want conflict", err)
	}
	requireCurrentScan(t, pool, id, true) // A rejected DO UPDATE must not invalidate.
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET sha256 = decode(repeat('ab', 32), 'hex') WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	requireCurrentScan(t, pool, id, true)
	dbtest.SetMediaAssetState(t, pool, id, "deleted")
	requireCurrentScan(t, pool, id, true)
	input.Assets[0].RelPath = "original/replacement.ts"
	replaced, err := media.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.AssetIDs[0] != id {
		t.Fatal("replacement must update the existing logical asset")
	}
	requireCurrentScan(t, pool, id, false)
	var markers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_ts_scans WHERE media_asset_id = $1`, id).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("markers after replacement = %d, want 0", markers)
	}
	// A statement with an older snapshot can miss a concurrently inserted marker.
	// Its old revision must still fail the view even when its size matches.
	if _, err := pool.Exec(ctx, `INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes, content_revision) VALUES ($1, 188, $2)`, id, source.ContentRevision); err != nil {
		t.Fatal(err)
	}
	requireCurrentScan(t, pool, id, false)
}

func TestTSScanRejectsInFlightInPlaceReplacement(t *testing.T) {
	for _, otherStore := range []bool{false, true} {
		name := "same_store"
		if otherStore {
			name = "other_store"
		}
		t.Run(name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := t.Context()
			media := testMedia(pool, dbtest.StoreName, "")
			input := scanOriginalInput("original/first.ts")
			result, err := media.RegisterInPlace(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			source, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
			if err != nil {
				t.Fatal(err)
			}
			writer := media
			if otherStore {
				writer = testMedia(pool, "other-store", "")
			} else {
				dbtest.SetMediaAssetState(t, pool, source.ID, "deleted")
			}
			input.Assets[0].RelPath = "original/replacement.ts"
			replaced, err := writer.RegisterInPlace(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if replaced.AssetIDs[0] != source.ID {
				t.Fatal("replacement did not update original")
			}
			ok, err := media.ReplaceScan(ctx, source, 188, []store.RecordingDropStat{{Pid: 256, Packets: 1}}, nil)
			if err != nil || ok {
				t.Fatalf("stale ReplaceScan = %v, %v, want false, nil", ok, err)
			}
			var rows int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM media_asset_ts_scans WHERE media_asset_id = $1) + (SELECT count(*) FROM drop_stats WHERE media_asset_id = $1)`, source.ID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("stale scan wrote %d rows, want 0", rows)
			}
			fresh, err := writer.GetActiveOriginalForTSScan(ctx, result.RecordingID)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := writer.ReplaceScan(ctx, fresh, 188, nil, nil); err != nil || !ok {
				t.Fatalf("fresh ReplaceScan = %v, %v, want true, nil", ok, err)
			}
			requireCurrentScan(t, pool, source.ID, true)
		})
	}
}

func TestCatalogRestoresCurrentContentScan(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := testMedia(pool, dbtest.StoreName, "")
	result, err := media.RegisterInPlace(ctx, scanOriginalInput("original/catalog.ts"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := media.ReplaceScan(ctx, source, 188, nil, nil); err != nil || !ok {
		t.Fatalf("scan = %v, %v", ok, err)
	}
	catalog := NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName)
	doc, err := catalog.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	requireCurrentScan(t, pool, source.ID, true)
	doc.MediaAssetTSScans[0].ScannedSizeBytes = 187
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	requireCurrentScan(t, pool, source.ID, false)
	doc.MediaAssetTSScans = nil
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	requireCurrentScan(t, pool, source.ID, false)
}

func TestCatalogExportExcludesStaleContentScan(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	media := testMedia(pool, dbtest.StoreName, "")
	input := scanOriginalInput("original/export-first.ts")
	result, err := media.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	source, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.SetMediaAssetState(t, pool, source.ID, "deleted")
	input.Assets[0].RelPath = "original/export-replacement.ts"
	if _, err := media.RegisterInPlace(ctx, input); err != nil {
		t.Fatal(err)
	}
	// Model the old marker missed by the replacement statement's snapshot.
	if _, err := pool.Exec(ctx, `INSERT INTO media_asset_ts_scans
		(media_asset_id, scanned_size_bytes, content_revision) VALUES ($1, 188, $2)`, source.ID, source.ContentRevision); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName)
	doc, err := catalog.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.MediaAssetTSScans) != 0 {
		t.Fatalf("exported stale-content scans = %d, want 0", len(doc.MediaAssetTSScans))
	}
	// A current revision with a stale size must remain in the document.
	if _, err := pool.Exec(ctx, `UPDATE media_asset_ts_scans s
		SET content_revision = a.content_revision, scanned_size_bytes = 187
		FROM media_assets a WHERE a.id = s.media_asset_id AND a.id = $1`, source.ID); err != nil {
		t.Fatal(err)
	}
	doc, err = catalog.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.MediaAssetTSScans) != 1 || doc.MediaAssetTSScans[0].ScannedSizeBytes != 187 {
		t.Fatalf("exported current-revision scans = %+v, want one scan with size 187", doc.MediaAssetTSScans)
	}
}

func TestTSScanRejectsInFlightCatalogRestore(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	media := testMedia(pool, dbtest.StoreName, "")
	result, err := media.RegisterInPlace(ctx, scanOriginalInput("original/restore-during-scan.ts"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName)
	doc, err := catalog.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	ok, err := media.ReplaceScan(ctx, source, 188, []store.RecordingDropStat{{Pid: 256, Packets: 1}}, nil)
	if err != nil || ok {
		t.Fatalf("ReplaceScan after catalog restore = %v, %v, want false, nil", ok, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM media_asset_ts_scans WHERE media_asset_id = $1) +
		(SELECT count(*) FROM drop_stats WHERE media_asset_id = $1)`, source.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("scan after catalog restore wrote %d rows, want 0", rows)
	}
	fresh, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := media.ReplaceScan(ctx, fresh, 188, nil, nil); err != nil || !ok {
		t.Fatalf("fresh scan after catalog restore = %v, %v, want true, nil", ok, err)
	}
}

func TestCatalogRestoreRemovesAbsentTSScan(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	media := testMedia(pool, dbtest.StoreName, "")
	result, err := media.RegisterInPlace(ctx, scanOriginalInput("original/restore-without-scan.ts"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := media.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := media.ReplaceScan(ctx, source, 188, nil, nil); err != nil || !ok {
		t.Fatalf("initial scan = %v, %v, want true, nil", ok, err)
	}
	catalog := NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName)
	doc, err := catalog.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.MediaAssetTSScans) != 1 {
		t.Fatalf("exported scan count = %d, want 1", len(doc.MediaAssetTSScans))
	}
	doc.MediaAssetTSScans = nil
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	var markers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_ts_scans WHERE media_asset_id = $1`, source.ID).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("markers after catalog restore without scan = %d, want 0", markers)
	}
	requireCurrentScan(t, pool, source.ID, false)
}
