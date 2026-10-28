package pgstore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestNamedMediaViewsIgnoreStaleGenerationCopies(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	viewHasRecording := func(view string, recordingID int64) bool {
		t.Helper()
		var exists bool
		query := "SELECT EXISTS (SELECT 1 FROM " + view + " WHERE recording_id = $1)"
		if err := pool.QueryRow(ctx, query, recordingID).Scan(&exists); err != nil {
			t.Fatalf("checking %s for recording %d: %v", view, recordingID, err)
		}
		return exists
	}

	originalRecordingID := dbtest.Recording(t, pool)
	originalID := dbtest.MediaAsset(t, pool, originalRecordingID)
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET ingest_generation = 1 WHERE id = $1`, originalID); err != nil {
		t.Fatalf("advancing logical original generation: %v", err)
	}
	if viewHasRecording("encodable_recordings", originalRecordingID) {
		t.Fatal("encodable_recordings included a recording whose active original copy is stale")
	}
	setCopyGeneration(t, pool, originalID, 1)
	if !viewHasRecording("encodable_recordings", originalRecordingID) {
		t.Fatal("encodable_recordings excluded a recording after its original copy reached the current generation")
	}

	encodedRecordingID := dbtest.Recording(t, pool)
	dbtest.MediaAsset(t, pool, encodedRecordingID)
	profile := "h264"
	encodedID := dbtest.MediaAsset(t, pool, encodedRecordingID, func(params *dbtest.MediaAssetParams) {
		params.Kind = "encoded"
		params.Profile = &profile
	})
	keepRanges := "{[0,1000)}"
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
VALUES ($1, 'always', ARRAY['h264'])`, encodedRecordingID); err != nil {
		t.Fatalf("seeding desired encode profile: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
VALUES ($1, 'h264', $2::int8multirange)`, encodedRecordingID, keepRanges); err != nil {
		t.Fatalf("seeding encode rebuild request: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_cuts (media_asset_id, keep_ranges)
VALUES ($1, $2::int8multirange)`, encodedID, keepRanges); err != nil {
		t.Fatalf("seeding encoded cut ranges: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 2 WHERE id = $1`, encodedID); err != nil {
		t.Fatalf("advancing logical encoded generation: %v", err)
	}
	if !viewHasRecording("unencoded_desired_profiles", encodedRecordingID) {
		t.Fatal("unencoded_desired_profiles considered a stale encoded copy satisfied")
	}
	if !viewHasRecording("unsatisfied_encode_rebuilds", encodedRecordingID) {
		t.Fatal("unsatisfied_encode_rebuilds considered a stale encoded copy satisfied")
	}
	setCopyGeneration(t, pool, encodedID, 2)
	if viewHasRecording("unencoded_desired_profiles", encodedRecordingID) {
		t.Fatal("unencoded_desired_profiles reported a profile missing after the copy reached the current generation")
	}
	if viewHasRecording("unsatisfied_encode_rebuilds", encodedRecordingID) {
		t.Fatal("unsatisfied_encode_rebuilds reported a request unsatisfied after the current copy matched it")
	}

	cmRecordingID := dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
		params.NetworkID = 32001
		params.ServiceID = 1001
	})
	cmOriginalID := dbtest.MediaAsset(t, pool, cmRecordingID)
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', true)`, cmRecordingID); err != nil {
		t.Fatalf("enabling CM detection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
VALUES (32001, 1001, 0, 0, 10, 10, 100, 100)`); err != nil {
		t.Fatalf("seeding CM logo area: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET ingest_generation = 1 WHERE id = $1`, cmOriginalID); err != nil {
		t.Fatalf("advancing logical CM original generation: %v", err)
	}
	if viewHasRecording("cm_detection_desired", cmRecordingID) {
		t.Fatal("cm_detection_desired included a recording with a stale original copy")
	}
	var staleLogoCandidateExists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM cm_logo_candidate_desired
    WHERE network_id = 32001 AND service_id = 1001 AND recording_id IS NOT NULL
)`).Scan(&staleLogoCandidateExists); err != nil {
		t.Fatalf("checking stale-original logo candidate: %v", err)
	}
	if staleLogoCandidateExists {
		t.Fatal("cm_logo_candidate_desired returned a candidate with a stale original copy")
	}
	setCopyGeneration(t, pool, cmOriginalID, 1)
	if !viewHasRecording("cm_detection_desired", cmRecordingID) {
		t.Fatal("cm_detection_desired excluded a recording after its original copy reached the current generation")
	}
	var logoCandidateRecordingID int64
	if err := pool.QueryRow(ctx, `
SELECT recording_id FROM cm_logo_candidate_desired
WHERE network_id = 32001 AND service_id = 1001`).Scan(&logoCandidateRecordingID); err != nil {
		t.Fatalf("reading current-original logo candidate: %v", err)
	}
	if logoCandidateRecordingID != cmRecordingID {
		t.Fatalf("cm_logo_candidate_desired recording = %v, want %d", logoCandidateRecordingID, cmRecordingID)
	}

	removedRecordingID := dbtest.Recording(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
VALUES ($1, 'always', '{}')`, removedRecordingID); err != nil {
		t.Fatalf("seeding encode policy for removal: %v", err)
	}
	removedProfile := "h264"
	removedID := dbtest.MediaAsset(t, pool, removedRecordingID, func(params *dbtest.MediaAssetParams) {
		params.Kind = "encoded"
		params.Profile = &removedProfile
	})
	remainingProfile := "h265"
	remainingID := dbtest.MediaAsset(t, pool, removedRecordingID, func(params *dbtest.MediaAssetParams) {
		params.Kind = "encoded"
		params.Profile = &remainingProfile
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO encoded_asset_removal_requests (recording_id, profile)
VALUES ($1, 'h264')`, removedRecordingID); err != nil {
		t.Fatalf("seeding encoded asset removal request: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 1 WHERE id = $1`, remainingID); err != nil {
		t.Fatalf("advancing remaining encoded asset generation: %v", err)
	}
	if viewHasAsset(t, pool, removedID) {
		t.Fatal("removed_encoded_assets allowed deleting a profile when the only remaining encoded copy was stale")
	}
	setCopyGeneration(t, pool, remainingID, 1)
	if !viewHasAsset(t, pool, removedID) {
		t.Fatal("removed_encoded_assets excluded the target after another current encoded copy became available")
	}

	removedWithOriginalRecordingID := dbtest.Recording(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
VALUES ($1, 'always', '{}')`, removedWithOriginalRecordingID); err != nil {
		t.Fatalf("seeding encode policy for original-backed removal: %v", err)
	}
	staleOriginalID := dbtest.MediaAsset(t, pool, removedWithOriginalRecordingID)
	removedOriginalBackedID := dbtest.MediaAsset(t, pool, removedWithOriginalRecordingID, func(params *dbtest.MediaAssetParams) {
		params.Kind = "encoded"
		params.Profile = &removedProfile
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO encoded_asset_removal_requests (recording_id, profile)
VALUES ($1, 'h264')`, removedWithOriginalRecordingID); err != nil {
		t.Fatalf("seeding original-backed encoded asset removal request: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET ingest_generation = 1 WHERE id = $1`, staleOriginalID); err != nil {
		t.Fatalf("advancing remaining original generation: %v", err)
	}
	if viewHasAsset(t, pool, removedOriginalBackedID) {
		t.Fatal("removed_encoded_assets allowed deleting a profile when the only remaining original copy was stale")
	}
	setCopyGeneration(t, pool, staleOriginalID, 1)
	if !viewHasAsset(t, pool, removedOriginalBackedID) {
		t.Fatal("removed_encoded_assets excluded the target after the original copy reached the current generation")
	}
}

func setCopyGeneration(t *testing.T, pool *pgxpool.Pool, assetID, generation int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET generation = $2 WHERE media_asset_id = $1`, assetID, generation); err != nil {
		t.Fatalf("setting copy %d generation to %d: %v", assetID, generation, err)
	}
}

func viewHasAsset(t *testing.T, pool *pgxpool.Pool, assetID int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM removed_encoded_assets WHERE asset_id = $1)`, assetID).Scan(&exists); err != nil {
		t.Fatalf("checking removed_encoded_assets for asset %d: %v", assetID, err)
	}
	return exists
}
