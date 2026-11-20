package dbtest_test

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRemoveEncodeProfile_RemovesOnlyTheNamedDesiredProfile(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) {
		p.KeepOriginal = "until_encoded"
		p.EncodeProfiles = []string{"h264", "cut"}
	})
	// 版と作り直し要求は消さない（この builder は desired だけを外す）。
	dbtest.EncodedAssetRebuildRequest(t, pool, recordingID, "h264", []chapters.Range{{StartMs: 0, EndMs: 1000}})

	dbtest.RemoveEncodeProfile(t, pool, recordingID, "h264")
	var profiles []string
	var keepOriginal string
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT encode_profiles, keep_original FROM recording_processing_policy WHERE recording_id = $1`, recordingID).Scan(&profiles, &keepOriginal), "reading policy")
	if !slices.Equal(profiles, []string{"cut"}) || keepOriginal != "until_encoded" {
		t.Fatalf("policy after removing h264 = profiles %v keep_original %q, want [cut] until_encoded", profiles, keepOriginal)
	}

	// 最後の 1 つを外すと、until_encoded は always に倒れる（CHECK 制約と同じ向き）。
	dbtest.RemoveEncodeProfile(t, pool, recordingID, "cut")
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT encode_profiles, keep_original FROM recording_processing_policy WHERE recording_id = $1`, recordingID).Scan(&profiles, &keepOriginal), "reading emptied policy")
	if len(profiles) != 0 || keepOriginal != "always" {
		t.Fatalf("policy after removing the last profile = profiles %v keep_original %q, want empty and always", profiles, keepOriginal)
	}
	var rebuilds int
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, recordingID).Scan(&rebuilds), "counting rebuild requests")
	if rebuilds != 1 {
		t.Fatalf("rebuild requests after removal = %d, want 1 (the builder keeps them)", rebuilds)
	}
}
