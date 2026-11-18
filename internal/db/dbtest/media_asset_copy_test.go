package dbtest_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestMediaAssetCopy_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	read := func(id int64) (mediaAssetID int64, store, relPath string, generation int64, state string, createdAt time.Time, deletedAt *time.Time) {
		t.Helper()
		err := pool.QueryRow(t.Context(), `
SELECT media_asset_id, store, rel_path, generation, state, created_at, deleted_at
FROM media_asset_copies WHERE id = $1`, id).Scan(
			&mediaAssetID, &store, &relPath, &generation, &state, &createdAt, &deletedAt)
		testutil.RequireNoError(t, err, "reading media asset copy")
		return
	}

	// 既定値は、置き場 StoreName のコピーがまだ無い資産で確かめる（MediaAsset は既定の active コピーを作る）。
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(t.Context(), sqlcgen.CreateMediaAssetParams{
		RecordingID: dbtest.Recording(t, pool),
		Kind:        "original",
		SizeBytes:   1,
	})
	testutil.RequireNoError(t, err, "creating bare media asset")
	defaultID := dbtest.MediaAssetCopy(t, pool, assetID)
	gotAsset, gotStore, gotRelPath, gotGeneration, gotState, gotCreatedAt, gotDeletedAt := read(defaultID)
	if gotAsset != assetID || gotStore != dbtest.StoreName || gotRelPath != fmt.Sprintf("test-copy/%d.ts", assetID) ||
		gotGeneration != 0 || gotState != "active" || gotDeletedAt != nil || gotCreatedAt.IsZero() {
		t.Fatalf("default media asset copy = asset %d, store %q, rel_path %q, generation %d, state %q, deleted %v; want asset %d, store %q, test-copy/%d.ts, generation 0, active, not deleted",
			gotAsset, gotStore, gotRelPath, gotGeneration, gotState, gotDeletedAt, assetID, dbtest.StoreName, assetID)
	}

	createdAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	deletedAt := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	otherAssetID := dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool))
	overriddenID := dbtest.MediaAssetCopy(t, pool, otherAssetID, func(p *dbtest.MediaAssetCopyParams) {
		p.Store = "cloud"
		p.RelPath = "custom/copy.mp4"
		p.Generation = 3
		p.State = "deleted"
		p.CreatedAt = &createdAt
		p.DeletedAt = &deletedAt
	})
	gotAsset, gotStore, gotRelPath, gotGeneration, gotState, gotCreatedAt, gotDeletedAt = read(overriddenID)
	if gotAsset != otherAssetID || gotStore != "cloud" || gotRelPath != "custom/copy.mp4" ||
		gotGeneration != 3 || gotState != "deleted" || !gotCreatedAt.Equal(createdAt) ||
		gotDeletedAt == nil || !gotDeletedAt.Equal(deletedAt) {
		t.Fatalf("overridden media asset copy = asset %d, store %q, rel_path %q, generation %d, state %q, created %v, deleted %v; want asset %d, cloud, custom/copy.mp4, generation 3, deleted, created %v, deleted %v",
			gotAsset, gotStore, gotRelPath, gotGeneration, gotState, gotCreatedAt, gotDeletedAt, otherAssetID, createdAt, deletedAt)
	}
}
