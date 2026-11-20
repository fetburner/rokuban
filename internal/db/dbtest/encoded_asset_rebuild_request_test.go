package dbtest_test

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestEncodedAssetRebuildRequest_UpsertsKeep(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	// read は要求行の requested_keep を読み戻す。行が無ければ found=false。
	read := func(profile string) (keep []chapters.Range, found bool) {
		t.Helper()
		rows, err := pool.Query(t.Context(), `
SELECT lower(k), upper(k) FROM encoded_asset_rebuild_requests q CROSS JOIN LATERAL unnest(q.requested_keep) AS k
WHERE q.recording_id = $1 AND q.profile = $2 ORDER BY lower(k)`, recordingID, profile)
		testutil.RequireNoError(t, err, "reading rebuild request")
		defer rows.Close()
		for rows.Next() {
			var r chapters.Range
			testutil.RequireNoError(t, rows.Scan(&r.StartMs, &r.EndMs), "scanning rebuild request keep")
			keep = append(keep, r)
			found = true
		}
		testutil.RequireNoError(t, rows.Err(), "iterating rebuild request keep")
		return keep, found
	}

	if _, found := read("cut"); found {
		t.Fatal("rebuild request exists before the builder ran")
	}
	first := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	dbtest.EncodedAssetRebuildRequest(t, pool, recordingID, "cut", first)
	if got, found := read("cut"); !found || !slices.Equal(got, first) {
		t.Fatalf("rebuild request keep = %v (found %v), want %v", got, found, first)
	}

	// 同じ (録画, プロファイル) への再要求は keep を上書きする。
	second := []chapters.Range{{StartMs: 500, EndMs: 900}}
	dbtest.EncodedAssetRebuildRequest(t, pool, recordingID, "cut", second)
	if got, found := read("cut"); !found || !slices.Equal(got, second) {
		t.Fatalf("overwritten rebuild request keep = %v (found %v), want %v", got, found, second)
	}
}
