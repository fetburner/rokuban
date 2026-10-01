package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func playbackRequest(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var content *bytes.Reader
	if body == nil {
		content = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		content = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, content)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func createPlaybackRecording(t *testing.T, pool *pgxpool.Pool, start time.Time, eventID int32, title string) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source: "manual", Site: "default", NetworkID: 32736, ServiceID: 1024, EventID: eventID,
		ServiceName: "NHK総合", ChannelType: "GR", Channel: "27", Title: title,
		ProgramStartAt: start, ProgramDurationMs: time.Hour.Milliseconds(), Status: "finished",
	})
	if err != nil {
		t.Fatalf("create playback recording: %v", err)
	}
	return id
}

func TestPlaybackPositionAndWatchedAPI(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	start := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	one := createPlaybackRecording(t, pool, start, 100, "放送イベント")
	duplicate := createPlaybackRecording(t, pool, start, 101, "同じ開始時刻の別行")
	unrelated := createPlaybackRecording(t, pool, start.Add(time.Hour), 102, "別イベント")

	if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{RecordingID: one, PositionMs: 12_000}); err != nil {
		t.Fatalf("seed playback position: %v", err)
	}
	if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{RecordingID: duplicate, PositionMs: 22_000}); err != nil {
		t.Fatalf("seed duplicate playback position: %v", err)
	}

	resp := playbackRequest(t, http.MethodPut, fmt.Sprintf("%s/api/recordings/%d/playback-position", srv.URL, one), map[string]any{"positionMs": 1_000})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("small playback position status = %d, want 400", resp.StatusCode)
	}
	resp = playbackRequest(t, http.MethodPut, fmt.Sprintf("%s/api/recordings/%d/watched", srv.URL, one), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark watched status = %d, want 204", resp.StatusCode)
	}
	var markerCount, positionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_watched`).Scan(&markerCount); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_playback_positions WHERE recording_id = $1`, one).Scan(&positionCount); err != nil {
		t.Fatalf("count selected position: %v", err)
	}
	if markerCount != 1 || positionCount != 0 {
		t.Fatalf("after mark watched: markers=%d selected positions=%d, want 1 / 0", markerCount, positionCount)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_playback_positions WHERE recording_id = $1`, duplicate).Scan(&positionCount); err != nil {
		t.Fatalf("count duplicate position: %v", err)
	}
	if positionCount != 1 {
		t.Fatalf("marking one row cleared duplicate's position: count=%d, want 1", positionCount)
	}

	var rows []Recording
	resp = getJSON(t, srv.URL+"/api/recordings", &rows)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list recordings status = %d", resp.StatusCode)
	}
	for _, id := range []int64{one, duplicate} {
		found := false
		for _, row := range rows {
			if row.Id == id {
				found = true
				if row.WatchedAt == nil {
					t.Errorf("recording %d has no aggregated watchedAt", id)
				}
				if id == duplicate && (row.ResumePositionMs == nil || *row.ResumePositionMs != 22_000) {
					t.Errorf("duplicate resume position = %v, want 22000", row.ResumePositionMs)
				}
			}
		}
		if !found {
			t.Errorf("recording %d missing from list", id)
		}
	}

	// A watched marker on a trash/superseded sibling is removed when unwatching
	// any row from the same broadcast event.
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, duplicate); err != nil {
		t.Fatalf("mark sibling trash: %v", err)
	}
	resp = playbackRequest(t, http.MethodPut, fmt.Sprintf("%s/api/recordings/%d/watched", srv.URL, duplicate), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark trash sibling watched status = %d, want 204", resp.StatusCode)
	}
	resp = playbackRequest(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d/watched", srv.URL, one), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark event unwatched status = %d, want 204", resp.StatusCode)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_watched`).Scan(&markerCount); err != nil {
		t.Fatalf("count cleared markers: %v", err)
	}
	if markerCount != 0 {
		t.Fatalf("event markers after unwatched = %d, want 0", markerCount)
	}

	resp = playbackRequest(t, http.MethodPut, fmt.Sprintf("%s/api/recordings/%d/playback-position", srv.URL, unrelated), map[string]any{"positionMs": 15_000})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("save playback position status = %d, want 204", resp.StatusCode)
	}
	resp = playbackRequest(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d/playback-position", srv.URL, unrelated), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete playback position status = %d, want 204", resp.StatusCode)
	}
}

func TestListContinueWatchingOrderLimitAndWatchedGrouping(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]int64, 8)
	for i := range ids {
		ids[i] = createPlaybackRecording(t, pool, start.Add(time.Duration(i)*time.Hour), int32(200+i), fmt.Sprintf("番組%d", i))
		if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{
			RecordingID: ids[i], PositionMs: int64(5_000 + i*1_000),
		}); err != nil {
			t.Fatalf("seed position %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE recording_playback_positions SET updated_at = $2 WHERE recording_id = $1`, ids[i], start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("set updated_at %d: %v", i, err)
		}
	}
	for i, status := range []struct {
		title     string
		status    string
		deleted   bool
		superseded bool
	}{
		{title: "失敗", status: "failed"},
		{title: "ごみ箱", status: "finished", deleted: true},
		{title: "supersede", status: "finished", superseded: true},
	} {
		id := createPlaybackRecording(t, pool, start.Add(time.Duration(20+i)*time.Hour), int32(300+i), status.title)
		if _, err := pool.Exec(ctx, `UPDATE recordings SET status = $2, deleted_at = CASE WHEN $3 THEN now() ELSE NULL END, superseded_at = CASE WHEN $4 THEN now() ELSE NULL END WHERE id = $1`, id, status.status, status.deleted, status.superseded); err != nil {
			t.Fatalf("set excluded recording state %d: %v", id, err)
		}
		if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{RecordingID: id, PositionMs: 15_000}); err != nil {
			t.Fatalf("seed excluded position %d: %v", id, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE recording_playback_positions SET updated_at = now() + interval '1 hour' WHERE recording_id = $1`, id); err != nil {
			t.Fatalf("set excluded position timestamp %d: %v", id, err)
		}
	}
	// A sibling row with the same event key makes the event watched, suppressing
	// the otherwise resumable row from the home collection.
	sibling := createPlaybackRecording(t, pool, start.Add(7*time.Hour), 999, "視聴済みイベントの別行")
	if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{RecordingID: sibling, PositionMs: 9_000}); err != nil {
		t.Fatalf("seed sibling position: %v", err)
	}
	if _, err := q.UpsertRecordingWatched(ctx, sibling); err != nil {
		t.Fatalf("mark sibling watched: %v", err)
	}

	var got []Recording
	resp := getJSON(t, srv.URL+"/api/recordings/continue-watching", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("continue watching status = %d", resp.StatusCode)
	}
	if len(got) != 6 {
		t.Fatalf("continue watching rows = %d, want 6", len(got))
	}
	for i, row := range got {
		want := ids[6-i]
		if row.Id != want {
			t.Errorf("row %d id = %d, want %d (updated_at descending)", i, row.Id, want)
		}
		if row.ResumePositionMs == nil {
			t.Errorf("row %d omitted resumePositionMs", row.Id)
		}
	}
	for _, row := range got {
		if row.Id == ids[7] || row.Id == sibling {
			t.Errorf("watched event included in continue watching: %d", row.Id)
		}
	}
}
