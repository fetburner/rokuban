package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

type capacityPreviewOverage struct {
	Site        string    `json:"site"`
	StartAt     time.Time `json:"startAt"`
	EndAt       time.Time `json:"endAt"`
	Shortfall   int       `json:"shortfall"`
	JammedTypes []string  `json:"jammedTypes"`
}

type capacityPreviewProgram struct {
	ID        int64
	ServiceID int32
	EventID   int32
	Title     string
}

func insertPreviewTuner(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault)
VALUES ('default', 0, 'preview-tuner', ARRAY['GR'], true, false)`); err != nil {
		t.Fatalf("inserting preview tuner: %v", err)
	}
}

func insertPreviewProgram(t *testing.T, pool *pgxpool.Pool, programID int64, title, channel string, start time.Time) capacityPreviewProgram {
	t.Helper()
	serviceID := int32(20_000 + programID%20_000)
	eventID := int32(programID % 100_000)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_services (
  site, network_id, service_id, type, logo_id, remote_control_key_id,
  name, channel_type, channel, has_logo_data
) VALUES ('default', 32736, $1, 1, 0, 1, 'プレビュー局', 'GR', $2, false)
ON CONFLICT (site, network_id, service_id) DO UPDATE SET
  channel_type = EXCLUDED.channel_type, channel = EXCLUDED.channel`, serviceID, channel); err != nil {
		t.Fatalf("inserting preview service: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genres
) VALUES ('default', $1, 32736, $2, $3, $4::timestamptz, 1800000,
          $4::timestamptz + interval '30 minutes', true, $5, '', '[{"lv1":9}]')`,
		programID, serviceID, eventID, start, title); err != nil {
		t.Fatalf("inserting preview program: %v", err)
	}
	return capacityPreviewProgram{ID: programID, ServiceID: serviceID, EventID: eventID, Title: title}
}

func insertPreviewReservation(t *testing.T, pool *pgxpool.Pool, programID int64, channel string, start time.Time, ruleID *int64) {
	t.Helper()
	var ruleArg any
	if ruleID != nil {
		ruleArg = *ruleID
	}
	insertPreviewSnapshot(t, pool, programID, channel, start, int32(programID%100_000))
	if _, err := pool.Exec(context.Background(), `
INSERT INTO reservations (site, program_id, rule_id, base)
VALUES ('default', $1, $2, '{}')`, programID, ruleArg); err != nil {
		t.Fatalf("inserting preview reservation: %v", err)
	}
}

func insertPreviewSnapshot(t *testing.T, pool *pgxpool.Pool, programID int64, channel string, start time.Time, eventID int32) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
) VALUES ('default', $1, '既存予約', $2, 1800000, 32736, 5168, 'GR', $3, $4, '既存局')`,
		programID, start, channel, eventID); err != nil {
		t.Fatalf("inserting preview reservation snapshot: %v", err)
	}
}

func markPreviewNeverScheduled(t *testing.T, pool *pgxpool.Pool, program capacityPreviewProgram) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE program_snapshots
SET network_id = 32736, service_id = $2, event_id = $3
WHERE site = 'default' AND program_id = $1`, program.ID, program.ServiceID, program.EventID); err != nil {
		t.Fatalf("updating preview snapshot event identity: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO never_scheduled_events (site, network_id, service_id, event_id)
VALUES ('default', 32736, $1, $2)`, program.ServiceID, program.EventID); err != nil {
		t.Fatalf("inserting never-scheduled event: %v", err)
	}
}

func insertPreviewRule(t *testing.T, pool *pgxpool.Pool, dedupe bool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO rules (name, dedupe_enabled, dedupe_threshold)
VALUES ('capacity preview', $1, CASE WHEN $1 THEN 0.5 ELSE NULL END)
RETURNING id`, dedupe).Scan(&id); err != nil {
		t.Fatalf("inserting preview rule: %v", err)
	}
	return id
}

func insertPreviewRecording(t *testing.T, pool *pgxpool.Pool, ruleID *int64, program capacityPreviewProgram, title string, start time.Time, withOriginal bool) {
	t.Helper()
	var ruleArg any
	if ruleID != nil {
		ruleArg = *ruleID
	}
	var recordingID int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO recordings (
  rule_id, source, site, network_id, service_id, event_id,
  service_name, channel_type, channel, title,
  program_start_at, program_duration_ms, status
) VALUES ($1, 'rule', 'default', 32736, $2, $3,
          'プレビュー局', 'GR', '25', $4, $5, 1800000, 'finished')
RETURNING id`, ruleArg, program.ServiceID, program.EventID, title, start).Scan(&recordingID); err != nil {
		t.Fatalf("inserting preview recording: %v", err)
	}
	if withOriginal {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
VALUES ($1, 'original', $2, 1024, 'active')`, recordingID, fmt.Sprintf("preview/%d.m2ts", recordingID)); err != nil {
			t.Fatalf("inserting fulfilled original: %v", err)
		}
	}
}

func newCapacityPreviewServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, Sites: []string{"default"}}))
	t.Cleanup(srv.Close)
	return srv
}

func postCapacityPreview(t *testing.T, srv *httptest.Server, body map[string]any) []capacityPreviewOverage {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/capacity/preview", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/capacity/preview status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	var result []capacityPreviewOverage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPreviewCapacityOverages_AddsDistinctSearchCandidate(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	insertPreviewReservation(t, pool, 50001, "27", start, nil)
	insertPreviewProgram(t, pool, 50002, "候補番組", "25", start)

	got := postCapacityPreview(t, srv, map[string]any{"genres": []int{searchFixtureGenre}})
	if len(got) != 1 || got[0].Shortfall != 1 || len(got[0].JammedTypes) != 1 || got[0].JammedTypes[0] != "GR" {
		t.Fatalf("preview = %+v, want one GR overage with shortfall 1", got)
	}
	if !got[0].StartAt.Equal(start) || !got[0].EndAt.Equal(start.Add(30*time.Minute)) {
		t.Errorf("interval = %v..%v, want %v..%v", got[0].StartAt, got[0].EndAt, start, start.Add(30*time.Minute))
	}
}

func TestPreviewCapacityOverages_SamePhysicalChannelDoesNotAddDemand(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	insertPreviewReservation(t, pool, 50011, "27", start, nil)
	insertPreviewProgram(t, pool, 50012, "同じ物理チャンネル", "27", start)

	if got := postCapacityPreview(t, srv, map[string]any{"genres": []int{searchFixtureGenre}}); len(got) != 0 {
		t.Fatalf("preview = %+v, want none for a shared physical channel", got)
	}
}

func TestPreviewCapacityOverages_ExcludesReservedSkippedAndFulfilledCandidates(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	insertPreviewReservation(t, pool, 50021, "27", start, nil)

	// This reservation's saved channel differs from the current service projection. A candidate
	// query that ignores existing reservation rows would therefore add a distinct channel twice.
	reserved := insertPreviewProgram(t, pool, 50022, "予約済み", "25", start)
	insertPreviewReservation(t, pool, reserved.ID, "27", start, nil)

	skipped := insertPreviewProgram(t, pool, 50023, "skip 意図", "24", start)
	insertPreviewSnapshot(t, pool, skipped.ID, "24", start, skipped.EventID)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO program_intents (site, program_id, action)
VALUES ('default', $1, 'skip')`, skipped.ID); err != nil {
		t.Fatalf("inserting skip intent: %v", err)
	}

	fulfilled := insertPreviewProgram(t, pool, 50024, "録画済み", "23", start)
	insertPreviewRecording(t, pool, nil, fulfilled, fulfilled.Title, start, true)

	if got := postCapacityPreview(t, srv, map[string]any{"genres": []int{searchFixtureGenre}}); len(got) != 0 {
		t.Fatalf("preview = %+v, want none after excluding reserved, skipped, and fulfilled candidates", got)
	}
}

func TestPreviewCapacityOverages_UsesRulerDedupeForEditedRule(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	insertPreviewReservation(t, pool, 50031, "27", start, nil)
	ruleID := insertPreviewRule(t, pool, true)
	candidate := insertPreviewProgram(t, pool, 50032, "再放送スペシャル 第1話", "25", start)
	priorRecording := candidate
	priorRecording.EventID++
	insertPreviewRecording(t, pool, &ruleID, priorRecording, candidate.Title, start.Add(-24*time.Hour), false)

	got := postCapacityPreview(t, srv, map[string]any{
		"genres": []int{searchFixtureGenre},
		"ruleId": ruleID,
	})
	if len(got) != 0 {
		t.Fatalf("preview = %+v, want no added overage for a ruler-deduped candidate", got)
	}
}

func TestPreviewCapacityOverages_DedupeKeepsExplicitRecordIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	insertPreviewReservation(t, pool, 50035, "27", start, nil)
	ruleID := insertPreviewRule(t, pool, true)
	candidate := insertPreviewProgram(t, pool, 50036, "再放送スペシャル 第1話", "25", start)
	insertPreviewSnapshot(t, pool, candidate.ID, "25", start, candidate.EventID)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO program_intents (site, program_id, action)
VALUES ('default', $1, 'record')`, candidate.ID); err != nil {
		t.Fatalf("inserting record intent: %v", err)
	}

	priorRecording := candidate
	priorRecording.EventID++
	insertPreviewRecording(t, pool, &ruleID, priorRecording, candidate.Title, start.Add(-24*time.Hour), false)

	got := postCapacityPreview(t, srv, map[string]any{
		"genres": []int{searchFixtureGenre},
		"ruleId": ruleID,
	})
	if len(got) != 1 || got[0].Shortfall != 1 {
		t.Fatalf("preview = %+v, want one overage with shortfall 1 because record intent overrides dedupe", got)
	}
}

func TestPreviewCapacityOverages_ExcludesNeverScheduledEditedReservation(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	ruleID := insertPreviewRule(t, pool, false)
	candidate := insertPreviewProgram(t, pool, 50037, "未スケジュールの番組", "25", start)
	insertPreviewReservation(t, pool, candidate.ID, "25", start, &ruleID)
	markPreviewNeverScheduled(t, pool, candidate)
	insertPreviewReservation(t, pool, 50038, "27", start, nil)

	got := postCapacityPreview(t, srv, map[string]any{
		"genres": []int{searchFixtureGenre},
		"ruleId": ruleID,
	})
	if len(got) != 0 {
		t.Fatalf("preview = %+v, want no overage for a never-scheduled event", got)
	}
}

func TestPreviewCapacityOverages_RuleIdReservationCanBeReevaluatedAsCandidate(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityPreviewServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	insertPreviewTuner(t, pool)
	ruleID := insertPreviewRule(t, pool, false)
	insertPreviewReservation(t, pool, 50041, "25", start, &ruleID)
	insertPreviewReservation(t, pool, 50042, "27", start, nil)
	// The edited reservation snapshot still says channel 25, while its current EPG service
	// projects channel 24. The preview must remove the old reservation and add the match using
	// the current service identity. Otherwise the stale channel is counted as extra demand.
	insertPreviewProgram(t, pool, 50041, "編集対象の候補", "24", start)
	insertPreviewProgram(t, pool, 50043, "追加候補", "23", start)

	got := postCapacityPreview(t, srv, map[string]any{
		"genres": []int{searchFixtureGenre},
		"ruleId": ruleID,
	})
	if len(got) != 1 || got[0].Shortfall != 2 {
		t.Fatalf("preview = %+v, want one worsened interval with shortfall 2", got)
	}
}
