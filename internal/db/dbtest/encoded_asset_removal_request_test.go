package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestEncodedAssetRemovalRequest_AddsOnlyTheRequestAndIsIdempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) {
		p.EncodeProfiles = []string{"h264"}
	})
	dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = new("h264")
	})

	dbtest.EncodedAssetRemovalRequest(t, pool, recordingID, "h264")
	dbtest.EncodedAssetRemovalRequest(t, pool, recordingID, "h264")
	var requests int
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT count(*) FROM encoded_asset_removal_requests WHERE recording_id = $1 AND profile = 'h264'`, recordingID).Scan(&requests), "counting removal requests")
	if requests != 1 {
		t.Fatalf("removal requests = %d, want 1 (a repeated request is a no-op)", requests)
	}

	// 版と desired は残す（この builder は要求だけを置く）。
	var profiles []string
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT encode_profiles FROM recording_processing_policy WHERE recording_id = $1`, recordingID).Scan(&profiles), "reading policy")
	if len(profiles) != 1 || profiles[0] != "h264" {
		t.Fatalf("encode_profiles = %v, want [h264] untouched", profiles)
	}
	var versions int
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'h264'`, recordingID).Scan(&versions), "counting encoded versions")
	if versions != 1 {
		t.Fatalf("encoded versions = %d, want 1 untouched", versions)
	}
}
