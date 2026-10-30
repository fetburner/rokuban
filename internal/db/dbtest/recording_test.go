package dbtest_test

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRecording_DefaultsAndUniqueEventID(t *testing.T) {
	pool := testutil.SetupDB(t)
	firstID := dbtest.Recording(t, pool)
	secondID := dbtest.Recording(t, pool)
	first := readRecording(t, pool, firstID)
	second := readRecording(t, pool, secondID)
	if first.eventID == second.eventID {
		t.Fatalf("default event IDs = %d and %d, want unique IDs", first.eventID, second.eventID)
	}
	for _, got := range []recordingRow{first, second} {
		if got.eventID < 0 || got.eventID > 65535 {
			t.Errorf("default event ID = %d, want within 0..65535", got.eventID)
		}
		if got.title == "" || got.programStartAt.IsZero() || !got.programStartAt.Equal(got.programStartAt.Truncate(time.Second)) || got.durationMs <= 0 || got.status == "" {
			t.Errorf("invalid recording defaults: %+v", got)
		}
	}
}

func TestRecording_OptionsOverrideDefaults(t *testing.T) {
	pool := testutil.SetupDB(t)
	wantStart := time.Date(2026, 10, 23, 12, 34, 56, 0, time.UTC)
	id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.EventID = 56
		fixtureParams.Title = "explicit title"
		fixtureParams.ProgramStartAt = wantStart
		fixtureParams.ProgramDurationMs = 78_000
	})
	got := readRecording(t, pool, id)
	if got.eventID != 56 || got.title != "explicit title" || !got.programStartAt.Equal(wantStart) || got.durationMs != 78_000 {
		t.Fatalf("overridden recording = %+v", got)
	}
}

func TestFixtureWithError_ReturnsCreateErrors(t *testing.T) {
	pool := testutil.SetupDB(t)
	if _, err := dbtest.RecordingWithError(t.Context(), pool, func(params *dbtest.RecordingParams) {
		params.Status = "invalid"
	}); err == nil {
		t.Fatal("RecordingWithError() error = nil, want status constraint error")
	}
	if _, err := dbtest.MediaAssetWithError(t.Context(), pool, -1); err == nil {
		t.Fatal("MediaAssetWithError() error = nil, want foreign key error")
	}
}

func TestMediaAsset_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	var got struct {
		recordingID int64
		kind        string
		profile     *string
		relPath     string
		sizeBytes   int64
		state       string
	}
	if err := pool.QueryRow(context.Background(), `
SELECT a.recording_id, a.kind, a.profile, c.rel_path, a.size_bytes, c.state
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id
WHERE a.id = $1`, assetID).Scan(&got.recordingID, &got.kind, &got.profile, &got.relPath, &got.sizeBytes, &got.state); err != nil {
		t.Fatalf("reading default original asset: %v", err)
	}
	if got.recordingID != recordingID || got.kind != "original" || got.profile != nil || got.relPath == "" || got.sizeBytes <= 0 || got.state != "active" {
		t.Fatalf("default original asset = %+v", got)
	}

	overriddenRecordingID := dbtest.Recording(t, pool)
	overriddenID := dbtest.MediaAsset(t, pool, overriddenRecordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = "test/override.ts"
		fixtureParams.SizeBytes = 42
	})
	if err := pool.QueryRow(context.Background(), `
SELECT a.kind, a.profile, c.rel_path, a.size_bytes
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id
WHERE a.id = $1`, overriddenID).Scan(&got.kind, &got.profile, &got.relPath, &got.sizeBytes); err != nil {
		t.Fatalf("reading overridden media asset: %v", err)
	}
	if got.kind != "original" || got.profile != nil || got.relPath != "test/override.ts" || got.sizeBytes != 42 {
		t.Fatalf("overridden media asset = %+v", got)
	}
}

type recordingRow struct {
	eventID        int32
	title          string
	programStartAt time.Time
	durationMs     int64
	status         string
}

func readRecording(t *testing.T, pool sqlcgen.DBTX, id int64) recordingRow {
	t.Helper()
	var got recordingRow
	err := pool.QueryRow(context.Background(), `
SELECT event_id, title, program_start_at, program_duration_ms, status FROM recordings WHERE id = $1`, id).Scan(
		&got.eventID, &got.title, &got.programStartAt, &got.durationMs, &got.status,
	)
	if err != nil {
		t.Fatalf("reading recording %d: %v", id, err)
	}
	return got
}
