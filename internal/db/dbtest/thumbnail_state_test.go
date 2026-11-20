package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestThumbnailState_ReadsTheActiveRowAndSeekOrReportsAbsent(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	if _, found := dbtest.ThumbnailState(t, pool, recordingID, dbtest.StoreName); found {
		t.Fatal("thumbnail found before any row exists")
	}
	id := dbtest.DerivedMediaAsset(t, pool, recordingID, "thumbnail")
	dbtest.MediaAssetCopy(t, pool, id, func(p *dbtest.MediaAssetCopyParams) { p.RelPath = "thumbnails/state.jpg" })

	row, found := dbtest.ThumbnailState(t, pool, recordingID, dbtest.StoreName)
	if !found || row.ID != id || row.RelPath != "thumbnails/state.jpg" || row.SeekMs != nil {
		t.Fatalf("thumbnail state = %+v (found %v), want id %d, rel_path thumbnails/state.jpg, no seek", row, found, id)
	}
	testutil.MustExec(t, pool, "recording the thumbnail seek", `INSERT INTO media_asset_thumbnail_seeks (media_asset_id, seek_ms) VALUES ($1, 30000)`, id)
	if row, _ := dbtest.ThumbnailState(t, pool, recordingID, dbtest.StoreName); row.SeekMs == nil || *row.SeekMs != 30000 {
		t.Fatalf("thumbnail seek = %v, want 30000", row.SeekMs)
	}
	if _, found := dbtest.ThumbnailState(t, pool, recordingID, "other-store"); found {
		t.Fatal("thumbnail reported in a store without a copy")
	}
}
