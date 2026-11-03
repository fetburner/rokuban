package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueQualityEventsPreservesOrder(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := newTestMediaDir(t)
	events := json.RawMessage(`[
		{"at":"2026-07-29T12:01:00Z","event":"recording.failed","reason":{"type":"io-error","message":"disk full"}},
		{"at":"2026-07-29T12:03:00Z","event":"recording.failed","reason":{"type":"timeout","message":"latest failure"}}
	]`)
	doc := &Document{
		Version:    Version,
		ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "quality events",
			ProgramStartAt: fixedTime(), ProgramDurationMs: time.Hour.Milliseconds(), Status: "failed",
			QualityEvents: events, CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
	}
	genDir, err := Write(mediaDir, doc, 1, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := RescueFile(ctx, pgstoretest.Catalog(pool, []string{"default"}...), mediaDir, "test-store", filepath.Join(genDir, DocumentFilename)); err != nil {
		t.Fatalf("RescueFile: %v", err)
	}

	var gotJSON []byte
	if err := pool.QueryRow(ctx, `SELECT quality_events FROM recordings WHERE id = 1`).Scan(&gotJSON); err != nil {
		t.Fatalf("querying rescued quality_events: %v", err)
	}
	var got []struct {
		At     string `json:"at"`
		Event  string `json:"event"`
		Reason struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(gotJSON, &got); err != nil {
		t.Fatalf("unmarshalling rescued quality_events %s: %v", gotJSON, err)
	}
	want := []struct {
		at, message string
	}{
		{at: "2026-07-29T12:01:00Z", message: "disk full"},
		{at: "2026-07-29T12:03:00Z", message: "latest failure"},
	}
	if len(got) != len(want) {
		t.Fatalf("rescued quality_events = %s, want %d events", gotJSON, len(want))
	}
	for i, event := range got {
		if event.Event != "recording.failed" || event.At != want[i].at || event.Reason.Message != want[i].message {
			t.Errorf("rescued event %d = %+v, want failed at %s with message %q", i, event, want[i].at, want[i].message)
		}
	}
}

func TestRescueNullQualityEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := newTestMediaDir(t)

	doc := &Document{
		Version:    Version,
		ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "null events",
			ProgramStartAt: fixedTime(), ProgramDurationMs: time.Hour.Milliseconds(), Status: "failed",
			QualityEvents: json.RawMessage(`null`), CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
	}
	genDir, err := Write(mediaDir, doc, 1, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := RescueFile(ctx, pgstoretest.Catalog(pool, []string{"default"}...), mediaDir, "test-store", filepath.Join(genDir, DocumentFilename)); err != nil {
		t.Fatalf("RescueFile: %v", err)
	}

	var got string
	if err := pool.QueryRow(ctx, `SELECT quality_events::text FROM recordings WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("querying rescued quality_events: %v", err)
	}
	if got != "[]" {
		t.Errorf("rescued quality_events = %s, want []", got)
	}
}
