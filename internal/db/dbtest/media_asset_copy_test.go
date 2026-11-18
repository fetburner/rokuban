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
	// read は (store, rel_path) で行を読み戻す。builder は id を返さないので、この鍵で引く。
	read := func(store, relPath string) (mediaAssetID int64, generation int64, state string, createdAt time.Time, deletedAt *time.Time) {
		t.Helper()
		err := pool.QueryRow(t.Context(), `
SELECT media_asset_id, generation, state, created_at, deleted_at
FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, store, relPath).Scan(
			&mediaAssetID, &generation, &state, &createdAt, &deletedAt)
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
	dbtest.MediaAssetCopy(t, pool, assetID)
	relPath := fmt.Sprintf("test-copy/%d.ts", assetID)
	gotAsset, gotGeneration, gotState, gotCreatedAt, gotDeletedAt := read(dbtest.StoreName, relPath)
	if gotAsset != assetID || gotGeneration != 0 || gotState != "active" || gotDeletedAt != nil || gotCreatedAt.IsZero() {
		t.Fatalf("default media asset copy = asset %d, generation %d, state %q, deleted %v; want asset %d, generation 0, active, not deleted",
			gotAsset, gotGeneration, gotState, gotDeletedAt, assetID)
	}

	createdAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	deletedAt := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	otherAssetID := dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool))
	dbtest.MediaAssetCopy(t, pool, otherAssetID, func(p *dbtest.MediaAssetCopyParams) {
		p.Store = "cloud"
		p.RelPath = "custom/copy.mp4"
		p.Generation = 3
		p.State = "deleted"
		p.CreatedAt = &createdAt
		p.DeletedAt = &deletedAt
	})
	gotAsset, gotGeneration, gotState, gotCreatedAt, gotDeletedAt = read("cloud", "custom/copy.mp4")
	if gotAsset != otherAssetID || gotGeneration != 3 || gotState != "deleted" || !gotCreatedAt.Equal(createdAt) ||
		gotDeletedAt == nil || !gotDeletedAt.Equal(deletedAt) {
		t.Fatalf("overridden media asset copy = asset %d, generation %d, state %q, created %v, deleted %v; want asset %d, generation 3, deleted, created %v, deleted %v",
			gotAsset, gotGeneration, gotState, gotCreatedAt, gotDeletedAt, otherAssetID, createdAt, deletedAt)
	}
}
