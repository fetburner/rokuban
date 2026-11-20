package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestDerivedMediaAsset_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)

	// 既定値: profile なし、世代 0、サイズ 1。物理コピーは作らない。
	thumbID := dbtest.DerivedMediaAsset(t, pool, dbtest.Recording(t, pool), "thumbnail")
	var kind string
	var profile *string
	var generation, sizeBytes, copies int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT a.kind, a.profile, a.derived_generation, a.size_bytes,
       (SELECT count(*) FROM media_asset_copies c WHERE c.media_asset_id = a.id)
FROM media_assets a WHERE a.id = $1`, thumbID).Scan(&kind, &profile, &generation, &sizeBytes, &copies), "reading default derived asset")
	if kind != "thumbnail" || profile != nil || generation != 0 || sizeBytes != 1 || copies != 0 {
		t.Fatalf("default derived asset = kind %q profile %v generation %d size %d copies %d; want thumbnail, no profile, generation 0, size 1, no copies",
			kind, profile, generation, sizeBytes, copies)
	}

	// 上書き: profile、世代、サイズ。
	encodedID := dbtest.DerivedMediaAsset(t, pool, dbtest.Recording(t, pool), "encoded", func(p *dbtest.DerivedMediaAssetParams) {
		name := "cut"
		p.Profile = &name
		p.SizeBytes = 42
		p.Generation = 3
	})
	var gotProfile string
	var gotGeneration, gotSize int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT a.profile, a.derived_generation, a.size_bytes FROM media_assets a WHERE a.id = $1`, encodedID).Scan(&gotProfile, &gotGeneration, &gotSize), "reading overridden derived asset")
	if gotProfile != "cut" || gotGeneration != 3 || gotSize != 42 {
		t.Fatalf("overridden derived asset = profile %q generation %d size %d; want cut, 3, 42", gotProfile, gotGeneration, gotSize)
	}
}
