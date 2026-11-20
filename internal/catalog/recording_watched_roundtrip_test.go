package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestExportRescue_PreservesWatchedButOmitsPlaybackPosition(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.EventID = 701
		fixtureParams.ServiceName = "NHK総合"
		fixtureParams.Title = "視聴状態の復元"
		fixtureParams.ProgramStartAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		fixtureParams.ProgramDurationMs = time.Hour.Milliseconds()
	})

	watchedAt := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	playback := pgstoretest.Playback(pool)
	if err := playback.MarkWatched(ctx, recordingID); err != nil {
		t.Fatalf("mark watched: %v", err)
	}
	// MarkWatched は現在時刻を入れる。往復の検査は固定の視聴日時で行う。
	if _, err := pool.Exec(ctx, `UPDATE recording_watched SET watched_at = $2 WHERE recording_id = $1`, recordingID, watchedAt); err != nil {
		t.Fatalf("pinning watched_at: %v", err)
	}
	if err := playback.SavePosition(ctx, recordingID, 12_345); err != nil {
		t.Fatalf("save playback position: %v", err)
	}
	// 再生位置が実際に存在することを確かめてから書き出す。無いまま通ると、
	// 「書き出さない」の検査が空虚になる。
	var seededPositions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_playback_positions WHERE recording_id = $1`, recordingID).Scan(&seededPositions); err != nil || seededPositions != 1 {
		t.Fatalf("seeded playback positions = %d (err %v), want 1 before export", seededPositions, err)
	}

	doc, err := pgstoretest.Catalog(pool, []string{"default"}...).Export(ctx)
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

	mediaDir := newTestMediaDir(t)
	genDir, err := Write(mediaDir, doc, DefaultKeep, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM recording_watched`); err != nil {
		t.Fatalf("delete watched markers: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM recording_playback_positions`); err != nil {
		t.Fatalf("delete playback positions: %v", err)
	}
	result, err := RescueFile(ctx, pgstoretest.Catalog(pool, []string{"default"}...), mediaDir, "test-store", filepath.Join(genDir, DocumentFilename))
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
