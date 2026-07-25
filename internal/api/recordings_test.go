package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func seedRecording(t *testing.T, pool *pgxpool.Pool, title string, start time.Time, status string, eventID int32) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              defaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           eventID,
		ServiceName:       "ＯＨＫ",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             title,
		ProgramStartAt:    start,
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            status,
	})
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	return id
}

// seedIngested は録画に原本 media_asset と PID 別 drop_stats を付ける。
func seedIngested(t *testing.T, pool *pgxpool.Pool, recordingID, size int64, stats map[int32][4]int64) {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     fmt.Sprintf("test/%d.m2ts", recordingID),
		SizeBytes:   size,
	})
	if err != nil {
		t.Fatalf("seeding media_asset: %v", err)
	}
	for pid, s := range stats {
		if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          pid,
			Packets:      s[0],
			Drops:        s[1],
			Errors:       s[2],
			Scrambled:    s[3],
		}); err != nil {
			t.Fatalf("seeding drop_stat: %v", err)
		}
	}
}

func TestListRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	older := seedRecording(t, pool, "古い録画", base.Add(-2*time.Hour), "finished", 1)
	newer := seedRecording(t, pool, "新しい録画", base.Add(-time.Hour), "finished", 2)
	seedRecording(t, pool, "未 ingest", base.Add(-3*time.Hour), "recording", 3)

	seedIngested(t, pool, older, 1000, map[int32][4]int64{
		0x100: {500, 2, 1, 0},
		0x110: {300, 0, 0, 5},
	})
	seedIngested(t, pool, newer, 2000, map[int32][4]int64{
		0x100: {800, 0, 0, 0},
	})

	var got []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 3 {
		t.Fatalf("recordings = %d, want 3", len(got))
	}
	// program_start_at 降順
	if got[0].Title != "新しい録画" || got[1].Title != "古い録画" || got[2].Title != "未 ingest" {
		t.Errorf("order = %q, %q, %q", got[0].Title, got[1].Title, got[2].Title)
	}

	// ドロップ統計は PID 横断で合計される
	old := got[1]
	if old.DropSummary == nil {
		t.Fatal("ingested recording has no dropSummary")
	}
	want := DropSummary{Packets: 800, Drops: 2, Errors: 1, Scrambled: 5}
	if *old.DropSummary != want {
		t.Errorf("dropSummary = %+v, want %+v", *old.DropSummary, want)
	}
	if old.SizeBytes == nil || *old.SizeBytes != 1000 {
		t.Errorf("sizeBytes = %v, want 1000", old.SizeBytes)
	}

	// 未 ingest は「統計が全部 0」と区別できるよう dropSummary を省略する
	pending := got[2]
	if pending.DropSummary != nil {
		t.Errorf("un-ingested recording should omit dropSummary, got %+v", pending.DropSummary)
	}
	if pending.SizeBytes != nil {
		t.Errorf("un-ingested recording should omit sizeBytes, got %v", pending.SizeBytes)
	}
	if pending.Status != "recording" {
		t.Errorf("status = %q, want recording", pending.Status)
	}

	// 正常な録画も dropSummary は付く（全 0）
	if got[0].DropSummary == nil || *got[0].DropSummary != (DropSummary{Packets: 800}) {
		t.Errorf("clean recording dropSummary = %+v", got[0].DropSummary)
	}
}

func TestListRecordings_QualityEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := seedRecording(t, pool, "問題あり", time.Now().Truncate(time.Second), "failed", 1)

	events, err := json.Marshal([]db.QualityEvent{{At: time.Now(), Event: "bcas_anomaly"}})
	if err != nil {
		t.Fatalf("marshalling events: %v", err)
	}
	if err := sqlcgen.New(pool).AppendQualityEvents(context.Background(), sqlcgen.AppendQualityEventsParams{
		ID:     id,
		Events: events,
	}); err != nil {
		t.Fatalf("appending quality events: %v", err)
	}

	var got []Recording
	getJSON(t, srv.URL+"/api/recordings", &got)
	if len(got) != 1 {
		t.Fatalf("recordings = %d, want 1", len(got))
	}
	if got[0].QualityEvents == nil || len(*got[0].QualityEvents) != 1 {
		t.Fatalf("qualityEvents = %v, want 1 event", got[0].QualityEvents)
	}
	if (*got[0].QualityEvents)[0]["event"] != "bcas_anomaly" {
		t.Errorf("event = %v", (*got[0].QualityEvents)[0])
	}

	// イベントが無い録画では省略される
	seedRecording(t, pool, "正常", time.Now().Add(time.Hour).Truncate(time.Second), "finished", 2)
	got = nil
	getJSON(t, srv.URL+"/api/recordings", &got)
	for _, r := range got {
		if r.Title == "正常" && r.QualityEvents != nil {
			t.Errorf("clean recording should omit qualityEvents, got %v", r.QualityEvents)
		}
	}
}

func TestListRecordingDropStats(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := seedRecording(t, pool, "録画", time.Now().Truncate(time.Second), "finished", 1)
	seedIngested(t, pool, id, 1000, map[int32][4]int64{
		0x110: {300, 0, 0, 5},
		0x100: {500, 2, 1, 0},
	})

	var got []DropStat
	resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 2 {
		t.Fatalf("drop stats = %d, want 2", len(got))
	}
	// PID 昇順
	if got[0].Pid != 0x100 || got[1].Pid != 0x110 {
		t.Errorf("pid order = %d, %d", got[0].Pid, got[1].Pid)
	}
	if got[0].Drops != 2 || got[0].Errors != 1 {
		t.Errorf("stat[0] = %+v", got[0])
	}
	if got[1].Scrambled != 5 {
		t.Errorf("stat[1] = %+v", got[1])
	}

	// 未 ingest の録画は空配列（null ではない）
	bare := seedRecording(t, pool, "未 ingest", time.Now().Add(time.Hour).Truncate(time.Second), "recording", 2)
	var empty []DropStat
	resp = getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, bare), &empty)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if empty == nil {
		t.Error("drop stats should be an empty array, not null")
	}
	if len(empty) != 0 {
		t.Errorf("drop stats = %d, want 0", len(empty))
	}
}
