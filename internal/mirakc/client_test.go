package mirakc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGetVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"current":"3.5.0","latest":"3.5.0"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	v, err := c.GetVersion(context.Background())
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if v.Current != "3.5.0" {
		t.Errorf("current = %q, want %q", v.Current, "3.5.0")
	}
}

func TestListSchedules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/recording/schedules" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{"state":"scheduled","program":{"id":1,"eventId":1,"serviceId":1,"networkId":1,"isFree":true},"options":{"priority":1},"tags":["rokuban:reservation=42"]}]`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	schedules, err := c.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(schedules) != 1 {
		t.Fatalf("len = %d, want 1", len(schedules))
	}
	if schedules[0].State != "scheduled" {
		t.Errorf("state = %q, want %q", schedules[0].State, "scheduled")
	}
	id, ok := FindReservationID(schedules[0].Tags)
	if !ok || id != 42 {
		t.Errorf("reservation id = %d, %v, want 42, true", id, ok)
	}
}

func TestCreateSchedule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		var input ScheduleInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if input.ProgramID != 327360102415397 {
			t.Errorf("programId = %d, want 327360102415397", input.ProgramID)
		}
		if input.Options.Priority != 1 {
			t.Errorf("priority = %d, want 1", input.Options.Priority)
		}
		w.WriteHeader(http.StatusCreated)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"state":"scheduled","program":{"id":%d,"eventId":1,"serviceId":1,"networkId":1,"isFree":true},"options":{"priority":1},"tags":[]}`, input.ProgramID)
	}))
	defer srv.Close()

	contentPath := "videos/test.m2ts"
	c := NewClient(srv.URL, nil)
	s, err := c.CreateSchedule(context.Background(), ScheduleInput{
		ProgramID: 327360102415397,
		Options: Options{
			ContentPath: &contentPath,
			Priority:    1,
		},
		Tags: []string{ReservationTag(42)},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if s.State != "scheduled" {
		t.Errorf("state = %q, want %q", s.State, "scheduled")
	}
}

func TestDeleteSchedule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/api/recording/schedules/100" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	if err := c.DeleteSchedule(context.Background(), 100); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
}

func TestListRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{"id":"rec1","program":{"id":1,"eventId":1,"serviceId":1,"networkId":1,"isFree":true},"service":{"id":1,"serviceId":1,"networkId":1,"type":1,"name":"NHK","channel":{"type":"GR","channel":"27"},"hasLogoData":false},"tags":[],"recording":{"options":{"priority":1},"status":"finished","startTime":1700000000000},"content":{"path":"test.m2ts","type":"video/MP2T","length":1024}}]`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	records, err := c.ListRecords(context.Background())
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len = %d, want 1", len(records))
	}
	if records[0].Recording.Status != "finished" {
		t.Errorf("status = %q, want %q", records[0].Recording.Status, "finished")
	}
	if records[0].Content.Length == nil || *records[0].Content.Length != 1024 {
		t.Errorf("content.length = %v, want 1024", records[0].Content.Length)
	}
}

func TestGetRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/recording/records/rec1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"rec1","program":{"id":1,"eventId":1,"serviceId":1,"networkId":1,"isFree":true},"service":{"id":1,"serviceId":1,"networkId":1,"type":1,"name":"NHK","channel":{"type":"GR","channel":"27"},"hasLogoData":false},"tags":[],"recording":{"options":{"priority":1},"status":"finished","startTime":1700000000000},"content":{"path":"test.m2ts","type":"video/MP2T"}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	r, err := c.GetRecord(context.Background(), "rec1")
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if r.ID != "rec1" {
		t.Errorf("id = %q, want %q", r.ID, "rec1")
	}
}

func TestDeleteRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method: %s", r.Method)
		}
		purge := r.URL.Query().Get("purge")
		w.Header().Set("Content-Type", "application/json")
		if purge == "true" {
			_, _ = fmt.Fprint(w, `{"recordRemoved":true,"contentRemoved":true}`)
		} else {
			_, _ = fmt.Fprint(w, `{"recordRemoved":true,"contentRemoved":false}`)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	result, err := c.DeleteRecord(context.Background(), "rec1", true)
	if err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if !result.RecordRemoved || !result.ContentRemoved {
		t.Errorf("got %+v, want both true", result)
	}
}

func TestStreamRecord(t *testing.T) {
	content := strings.Repeat("A", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/recording/records/rec1/stream" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = fmt.Fprint(w, content[500:])
		} else {
			_, _ = fmt.Fprint(w, content)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)

	t.Run("full", func(t *testing.T) {
		body, _, err := c.StreamRecord(context.Background(), "rec1", 0)
		if err != nil {
			t.Fatalf("StreamRecord: %v", err)
		}
		defer func() { _ = body.Close() }()
		data, _ := io.ReadAll(body)
		if len(data) != 1000 {
			t.Errorf("len = %d, want 1000", len(data))
		}
	})

	t.Run("range", func(t *testing.T) {
		body, _, err := c.StreamRecord(context.Background(), "rec1", 500)
		if err != nil {
			t.Fatalf("StreamRecord with offset: %v", err)
		}
		defer func() { _ = body.Close() }()
		data, _ := io.ReadAll(body)
		if len(data) != 500 {
			t.Errorf("len = %d, want 500", len(data))
		}
	})
}

func TestHeadRecordStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Length", "999999")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	length, err := c.HeadRecordStream(context.Background(), "rec1")
	if err != nil {
		t.Fatalf("HeadRecordStream: %v", err)
	}
	if length != 999999 {
		t.Errorf("length = %d, want 999999", length)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	_, err := c.GetVersion(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", apiErr.StatusCode, http.StatusNotFound)
	}
}

func TestSubscribeSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not Flusher")
		}

		events := []struct {
			eventType string
			data      string
		}{
			{"recording.record-saved", `{"recordId":"rec1","recordingStatus":"finished"}`},
			{"recording.failed", `{"programId":100,"reason":{"type":"io-error","message":"disk full","osError":28}}`},
			{"recording.record-broken", `{"recordId":"rec2","reason":"content-file-missing"}`},
			{"epg.programs-updated", `{"serviceId":400101}`},
		}
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "event:%s\ndata:%s\n\n", e.eventType, e.data)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	ch := make(chan Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.Subscribe(ctx, ch, &SSEConfig{
			InitialBackoff: 100 * time.Millisecond,
			MaxBackoff:     500 * time.Millisecond,
		})
	}()

	var events []Event
	for i := 0; i < 4; i++ {
		select {
		case e := <-ch:
			events = append(events, e)
		case <-ctx.Done():
			t.Fatal("timed out waiting for events")
		}
	}
	cancel()
	wg.Wait()

	if events[0].Type != "recording.record-saved" {
		t.Errorf("event[0].type = %q, want %q", events[0].Type, "recording.record-saved")
	}
	var saved RecordSavedData
	if err := json.Unmarshal(events[0].Data, &saved); err != nil {
		t.Fatalf("unmarshal record-saved: %v", err)
	}
	if saved.RecordID != "rec1" || saved.RecordingStatus != "finished" {
		t.Errorf("record-saved = %+v", saved)
	}

	if events[1].Type != "recording.failed" {
		t.Errorf("event[1].type = %q, want %q", events[1].Type, "recording.failed")
	}
	var failed RecordingFailedData
	if err := json.Unmarshal(events[1].Data, &failed); err != nil {
		t.Fatalf("unmarshal recording.failed: %v", err)
	}
	if failed.ProgramID != 100 || failed.Reason.Type != "io-error" {
		t.Errorf("recording.failed = %+v", failed)
	}

	if events[2].Type != "recording.record-broken" {
		t.Errorf("event[2].type = %q, want %q", events[2].Type, "recording.record-broken")
	}
	var broken RecordBrokenData
	if err := json.Unmarshal(events[2].Data, &broken); err != nil {
		t.Fatalf("unmarshal record-broken: %v", err)
	}
	if broken.RecordID != "rec2" || broken.Reason != "content-file-missing" {
		t.Errorf("record-broken = %+v", broken)
	}

	if events[3].Type != "epg.programs-updated" {
		t.Errorf("event[3].type = %q, want %q", events[3].Type, "epg.programs-updated")
	}
}

func TestSubscribeSSE_Reconnect(t *testing.T) {
	var mu sync.Mutex
	connectCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectCount++
		count := connectCount
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		_, _ = fmt.Fprintf(w, "event:recording.record-saved\ndata:{\"recordId\":\"r%d\",\"recordingStatus\":\"finished\"}\n\n", count)
		flusher.Flush()
		// サーバーが即座に接続を切ることで再接続をテスト
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	ch := make(chan Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = c.Subscribe(ctx, ch, &SSEConfig{
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     100 * time.Millisecond,
		})
	}()

	// 少なくとも 2 回の接続からイベントを受信できることを確認
	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			var data RecordSavedData
			_ = json.Unmarshal(e.Data, &data)
			seen[data.RecordID] = true
		case <-ctx.Done():
			t.Fatalf("timed out, only received %d events", i)
		}
	}
	cancel()

	if !seen["r1"] || !seen["r2"] {
		t.Errorf("expected events from reconnections, got %v", seen)
	}
}

func TestMillisecondsNull(t *testing.T) {
	var ms Milliseconds
	if err := json.Unmarshal([]byte("null"), &ms); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if !ms.Time().IsZero() {
		t.Errorf("expected zero time for null, got %v", ms.Time())
	}
}

func TestMilliseconds(t *testing.T) {
	input := `1700000000000`
	var ms Milliseconds
	if err := json.Unmarshal([]byte(input), &ms); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := ms.Time()
	want := time.UnixMilli(1700000000000)
	if !got.Equal(want) {
		t.Errorf("time = %v, want %v", got, want)
	}

	out, err := json.Marshal(ms)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != input {
		t.Errorf("marshal = %s, want %s", out, input)
	}
}
