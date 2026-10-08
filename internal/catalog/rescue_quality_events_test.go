package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueLegacyQualityEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()

	legacyEvents := json.RawMessage(`[
		{"at":"2026-07-29T12:01:00Z","event":"recording.failed","reason":{"type":"io-error","message":"disk full"}},
		{"at":"2026-07-29T12:02:00Z","event":"bcas_anomaly"},
		{"at":"2026-07-29T12:03:00Z","event":"recording.failed","reason":{"type":"timeout","message":"latest failure"}},
		{"at":"2026-07-29T12:04:00Z","event":"bcas_anomaly"}
	]`)
	doc := &Document{
		Version:    Version,
		ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "legacy events",
			ProgramStartAt: fixedTime(), ProgramDurationMs: time.Hour.Milliseconds(), Status: "failed",
			QualityEvents: legacyEvents, CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
	}
	genDir, err := Write(mediaDir, doc, 1)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := RescueFile(ctx, pool, mediaDir, filepath.Join(genDir, DocumentFilename)); err != nil {
		t.Fatalf("RescueFile: %v", err)
	}

	var gotJSON []byte
	if err := pool.QueryRow(ctx, `SELECT quality_events FROM recordings WHERE id = 1`).Scan(&gotJSON); err != nil {
		t.Fatalf("querying rescued quality_events: %v", err)
	}
	var got []struct {
		At     time.Time `json:"at"`
		Event  string    `json:"event"`
		Reason struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(gotJSON, &got); err != nil {
		t.Fatalf("unmarshalling rescued quality_events %s: %v", gotJSON, err)
	}
	if len(got) != 2 {
		t.Fatalf("rescued quality_events = %s, want both failure events without bcas_anomaly", gotJSON)
	}
	if got[0].Event != "recording.failed" || got[0].At.Format(time.RFC3339) != "2026-07-29T12:01:00Z" ||
		got[0].Reason.Type != "io-error" || got[0].Reason.Message != "disk full" {
		t.Errorf("first rescued quality event = %+v, want first failure with its original reason", got[0])
	}
	if got[1].Event != "recording.failed" || got[1].At.Format(time.RFC3339) != "2026-07-29T12:03:00Z" ||
		got[1].Reason.Type != "timeout" || got[1].Reason.Message != "latest failure" {
		t.Errorf("last rescued quality event = %+v, want latest failure so home can show its reason", got[1])
	}
}
