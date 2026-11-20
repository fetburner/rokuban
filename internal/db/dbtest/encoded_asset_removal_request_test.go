package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestEncodedAssetRemovalRequest_InsertsOnceAndIsIdempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	// count は要求行の数を返す。
	count := func(profile string) int {
		t.Helper()
		var n int
		testutil.RequireNoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM encoded_asset_removal_requests WHERE recording_id = $1 AND profile = $2`,
			recordingID, profile).Scan(&n), "counting removal requests")
		return n
	}

	if got := count("h264"); got != 0 {
		t.Fatalf("removal requests before the builder ran = %d, want 0", got)
	}
	dbtest.EncodedAssetRemovalRequest(t, pool, recordingID, "h264")
	if got := count("h264"); got != 1 {
		t.Fatalf("removal requests after the builder ran = %d, want 1", got)
	}
	// 同じ (録画, プロファイル) への再要求は行を増やさない。
	dbtest.EncodedAssetRemovalRequest(t, pool, recordingID, "h264")
	if got := count("h264"); got != 1 {
		t.Fatalf("removal requests after a repeated request = %d, want 1", got)
	}
	// 別のプロファイルの要求は別の行になる。
	dbtest.EncodedAssetRemovalRequest(t, pool, recordingID, "h265")
	if got := count("h265"); got != 1 {
		t.Fatalf("removal requests for another profile = %d, want 1", got)
	}
}
