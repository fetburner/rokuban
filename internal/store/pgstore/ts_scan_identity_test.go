package pgstore

import (
	"testing"

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

// TestCatalogExportExcludesStaleContentScan は、古い内容のリビジョンの印を Postgres のスナップショットの窓で残した場合の export を検査する。
// 印を直接挿入して窓を再現するので、方言に依らない契約（TSScan の in-place 登録の検査）には置かない。
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
