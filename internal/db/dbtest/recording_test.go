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

	if first.id == second.id {
		t.Fatalf("recording IDs = %d and %d, want distinct rows", first.id, second.id)
	}
	if first.eventID == second.eventID {
		t.Fatalf("default event IDs = %d and %d, want unique IDs", first.eventID, second.eventID)
	}
	for _, got := range []recordingRow{first, second} {
		if got.site == "" || got.networkID == 0 || got.serviceID == 0 || got.serviceName == "" || got.channelType != "GR" || got.channel == "" {
			t.Errorf("invalid required defaults: %+v", got)
		}
		validStatus := got.status == "recording" || got.status == "finished" || got.status == "canceled" || got.status == "failed"
		if got.title == "" || got.programStartAt.IsZero() || !got.programStartAt.Equal(got.programStartAt.Truncate(time.Second)) || got.durationMs <= 0 || !validStatus {
			t.Errorf("invalid recording defaults: %+v", got)
		}
	}
}

func TestRecording_OptionsOverrideDefaults(t *testing.T) {
	pool := testutil.SetupDB(t)
	wantStart := time.Date(2026, 10, 23, 12, 34, 56, 0, time.UTC)
	id := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
		fixtureParams.Site = "override-site"
		fixtureParams.NetworkID = 12
		fixtureParams.ServiceID = 34
		fixtureParams.EventID = 56
		fixtureParams.Title = "explicit title"
		fixtureParams.ProgramStartAt = wantStart
		fixtureParams.ProgramDurationMs = 78_000
		fixtureParams.Status = "recording"
	})
	got := readRecording(t, pool, id)
	if got.site != "override-site" || got.networkID != 12 || got.serviceID != 34 || got.eventID != 56 || got.title != "explicit title" || !got.programStartAt.Equal(wantStart) || got.durationMs != 78_000 || got.status != "recording" {
		t.Fatalf("overridden recording = %+v", got)
	}
}

func TestOriginalAsset_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.OriginalAsset(t, pool, recordingID)
	var got struct {
		recordingID int64
		kind        string
		profile     *string
		relPath     string
		sizeBytes   int64
		state       string
	}
	if err := pool.QueryRow(context.Background(), `
SELECT recording_id, kind, profile, rel_path, size_bytes, state
FROM media_assets WHERE id = $1`, assetID).Scan(&got.recordingID, &got.kind, &got.profile, &got.relPath, &got.sizeBytes, &got.state); err != nil {
		t.Fatalf("reading default original asset: %v", err)
	}
	if got.recordingID != recordingID || got.kind != "original" || got.profile != nil || got.relPath == "" || got.sizeBytes <= 0 || got.state != "active" {
		t.Fatalf("default original asset = %+v", got)
	}

	overriddenRecordingID := dbtest.Recording(t, pool)
	overriddenID := dbtest.OriginalAsset(t, pool, overriddenRecordingID, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.RelPath = "test/override.ts"
		fixtureParams.SizeBytes = 42
	})
	if err := pool.QueryRow(context.Background(), `
SELECT kind, profile, rel_path, size_bytes
FROM media_assets WHERE id = $1`, overriddenID).Scan(&got.kind, &got.profile, &got.relPath, &got.sizeBytes); err != nil {
		t.Fatalf("reading overridden media asset: %v", err)
	}
	if got.kind != "original" || got.profile != nil || got.relPath != "test/override.ts" || got.sizeBytes != 42 {
		t.Fatalf("overridden media asset = %+v", got)
	}
}

type recordingRow struct {
	id             int64
	site           string
	networkID      int32
	serviceID      int32
	eventID        int32
	serviceName    string
	channelType    string
	channel        string
	title          string
	programStartAt time.Time
	durationMs     int64
	status         string
}

func readRecording(t *testing.T, pool sqlcgen.DBTX, id int64) recordingRow {
	t.Helper()
	var got recordingRow
	err := pool.QueryRow(context.Background(), `
SELECT id, site, network_id, service_id, event_id, service_name, channel_type,
       channel, title, program_start_at, program_duration_ms, status
FROM recordings WHERE id = $1`, id).Scan(
		&got.id, &got.site, &got.networkID, &got.serviceID, &got.eventID, &got.serviceName,
		&got.channelType, &got.channel, &got.title, &got.programStartAt, &got.durationMs, &got.status,
	)
	if err != nil {
		t.Fatalf("reading recording %d: %v", id, err)
	}
	return got
}
