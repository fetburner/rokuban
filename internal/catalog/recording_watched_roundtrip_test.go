package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestExportRescue_PreservesWatchedButOmitsPlaybackPosition(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source: "manual", Site: "default", NetworkID: 32736, ServiceID: 1024, EventID: 701,
		ServiceName: "NHK総合", ChannelType: "GR", Channel: "27", Title: "視聴状態の復元",
		ProgramStartAt:    time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		ProgramDurationMs: time.Hour.Milliseconds(), Status: "finished",
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
	watchedAt := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	if err := q.CatalogUpsertRecordingWatched(ctx, sqlcgen.CatalogUpsertRecordingWatchedParams{
		RecordingID: recordingID, WatchedAt: watchedAt,
	}); err != nil {
		t.Fatalf("insert watched marker: %v", err)
	}
	if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{
		RecordingID: recordingID, PositionMs: 12_345,
	}); err != nil {
		t.Fatalf("insert playback position: %v", err)
	}

	doc, err := Export(ctx, pool)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if doc.Version != Version || len(doc.RecordingWatched) != 1 || doc.RecordingWatched[0].RecordingID != recordingID {
		t.Fatalf("export watched markers = %+v at version %d", doc.RecordingWatched, doc.Version)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal catalog keys: %v", err)
	}
	if _, ok := keys["recordingPlaybackPositions"]; ok {
		t.Fatal("transient playback positions were included in catalog")
	}

	mediaDir := t.TempDir()
	genDir, err := Write(mediaDir, doc, DefaultKeep)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM recording_watched`); err != nil {
		t.Fatalf("delete watched markers: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM recording_playback_positions`); err != nil {
		t.Fatalf("delete playback positions: %v", err)
	}
	result, err := RescueFile(ctx, pool, mediaDir, filepath.Join(genDir, DocumentFilename))
	if err != nil {
		t.Fatalf("RescueFile: %v", err)
	}
	if result.RecordingWatched != 1 {
		t.Fatalf("rescued watched markers = %d, want 1", result.RecordingWatched)
	}
	var got time.Time
	if err := pool.QueryRow(ctx, `SELECT watched_at FROM recording_watched WHERE recording_id = $1`, recordingID).Scan(&got); err != nil {
		t.Fatalf("query restored marker: %v", err)
	}
	if !got.Equal(watchedAt) {
		t.Errorf("restored watched_at = %s, want %s", got, watchedAt)
	}
	var positionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_playback_positions WHERE recording_id = $1`, recordingID).Scan(&positionCount); err != nil {
		t.Fatalf("query playback positions: %v", err)
	}
	if positionCount != 0 {
		t.Errorf("playback positions restored = %d, want 0", positionCount)
	}
}
