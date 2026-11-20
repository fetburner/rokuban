package dbtest_test

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestSetMediaAssetCuts_ReplacesFrozenRanges(t *testing.T) {
	pool := testutil.SetupDB(t)
	assetID := dbtest.DerivedMediaAsset(t, pool, dbtest.Recording(t, pool), "encoded", func(p *dbtest.DerivedMediaAssetParams) {
		name := "cut"
		p.Profile = &name
	})
	// read は凍結区間を [start, end) の列で読み戻す。区間が無ければ nil。
	read := func() []chapters.Range {
		t.Helper()
		rows, err := pool.Query(t.Context(), `
SELECT lower(k), upper(k) FROM media_asset_cuts c CROSS JOIN LATERAL unnest(c.keep_ranges) AS k
WHERE c.media_asset_id = $1 ORDER BY lower(k)`, assetID)
		testutil.RequireNoError(t, err, "reading frozen ranges")
		defer rows.Close()
		var out []chapters.Range
		for rows.Next() {
			var r chapters.Range
			testutil.RequireNoError(t, rows.Scan(&r.StartMs, &r.EndMs), "scanning frozen range")
			out = append(out, r)
		}
		testutil.RequireNoError(t, rows.Err(), "iterating frozen ranges")
		return out
	}

	first := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 1500, EndMs: 2000}}
	dbtest.SetMediaAssetCuts(t, pool, assetID, first)
	if got := read(); !slices.Equal(got, first) {
		t.Fatalf("frozen ranges after first set = %v, want %v", got, first)
	}

	// 置き換えは既存の行を消してから入れる。前の区間は残らない。空の区間は捨てる。
	second := []chapters.Range{{StartMs: 5000, EndMs: 6000}, {StartMs: 7000, EndMs: 7000}}
	dbtest.SetMediaAssetCuts(t, pool, assetID, second)
	if got := read(); !slices.Equal(got, []chapters.Range{{StartMs: 5000, EndMs: 6000}}) {
		t.Fatalf("frozen ranges after replacement = %v, want only [5000, 6000)", got)
	}
}
