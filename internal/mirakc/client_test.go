package mirakc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type slogRecordWriter struct {
	records chan<- string
}

func (w slogRecordWriter) Write(p []byte) (int, error) {
	w.records <- string(p)
	return len(p), nil
}

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
		_, _ = fmt.Fprint(w, `[{"state":"scheduled","program":{"id":1,"eventId":1,"serviceId":1,"networkId":1,"isFree":true},"options":{"priority":1},"tags":["program:42"]}]`)
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
	id, ok := FindProgramTag(schedules[0].Tags)
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
		Tags: []string{ProgramTag(42)},
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

// streamRangeServer は mirakc の records/{id}/stream を Range 前提で模す。
// status が http.StatusOK なら「Range を無視して全量を 200 で返す」サーバ、
// 204 / 416 ならその status をそのまま返すサーバになる。status 0 は Range 準拠。
func streamRangeServer(t *testing.T, content []byte, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/recording/records/rec1/stream" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch status {
		case http.StatusOK:
			// Range を無視して完全な表現を返す（RFC 9110 が許す挙動）。
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
			return
		case 0:
			// Range 準拠。
		default:
			w.WriteHeader(status)
			return
		}
		first, ok := parseRangeFirst(r.Header.Get("Range"))
		if !ok {
			t.Errorf("Range ヘッダが無い。offset 0 でも Range を送る契約（応答が tail -f 経路に落ちる）")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
			return
		}
		if first >= int64(len(content)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body := content[first:]
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// parseRangeFirst は `bytes=N-` の N を返す。
func parseRangeFirst(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, false
	}
	numStr, ok := strings.CutSuffix(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func TestStreamRecord(t *testing.T) {
	content := []byte(strings.Repeat("A", 1000))

	t.Run("offset 0 でも Range を送り 206 を受ける", func(t *testing.T) {
		c := NewClient(streamRangeServer(t, content, 0).URL, nil)
		body, length, err := c.StreamRecord(context.Background(), "rec1", 0)
		if err != nil {
			t.Fatalf("StreamRecord(offset=0): %v", err)
		}
		defer func() { _ = body.Close() }()
		data, _ := io.ReadAll(body)
		if len(data) != 1000 {
			t.Errorf("len = %d, want 1000", len(data))
		}
		if length != 1000 {
			t.Errorf("Content-Length = %d, want 1000", length)
		}
	})

	t.Run("offset>0 は差分を返す", func(t *testing.T) {
		c := NewClient(streamRangeServer(t, content, 0).URL, nil)
		body, _, err := c.StreamRecord(context.Background(), "rec1", 500)
		if err != nil {
			t.Fatalf("StreamRecord(offset=500): %v", err)
		}
		defer func() { _ = body.Close() }()
		data, _ := io.ReadAll(body)
		if len(data) != 500 {
			t.Errorf("len = %d, want 500", len(data))
		}
	})

	// 204 は「content file がまだ 0 バイト」を表す正常な応答である。録画の
	// 最初の 1〜2 秒は必ずここを通るので、接続失敗として数えてはならない。
	t.Run("204 は ErrRecordNotReady", func(t *testing.T) {
		c := NewClient(streamRangeServer(t, content, http.StatusNoContent).URL, nil)
		_, _, err := c.StreamRecord(context.Background(), "rec1", 0)
		if !errors.Is(err, ErrRecordNotReady) {
			t.Fatalf("err = %v, want ErrRecordNotReady", err)
		}
	})

	// 416 は追い付いた状態（offset >= 現在サイズ）を表す正常な応答である
	// --- 録画中は `ContentRange::without_size` が first/last を検査しないため
	// 206 + 0 バイト、完了後は `with_size` が弾いて 416 になる。
	t.Run("416 は ErrRangeNotSatisfiable", func(t *testing.T) {
		c := NewClient(streamRangeServer(t, content, http.StatusRequestedRangeNotSatisfiable).URL, nil)
		_, _, err := c.StreamRecord(context.Background(), "rec1", 0)
		if !errors.Is(err, ErrRangeNotSatisfiable) {
			t.Fatalf("err = %v, want ErrRangeNotSatisfiable", err)
		}
	})

	// RFC 9110 はサーバが Range を無視して 200 で完全な表現を返すことを許す。
	// offset 0 ではその本文が求めた差分と同一なので受理する。
	t.Run("offset 0 の 200 は受理する（Range 無視）", func(t *testing.T) {
		c := NewClient(streamRangeServer(t, content, http.StatusOK).URL, nil)
		body, _, err := c.StreamRecord(context.Background(), "rec1", 0)
		if err != nil {
			t.Fatalf("StreamRecord(offset=0) の 200: %v", err)
		}
		defer func() { _ = body.Close() }()
		data, _ := io.ReadAll(body)
		if len(data) != 1000 {
			t.Errorf("len = %d, want 1000", len(data))
		}
	})

	// offset > 0 の 200 は本文が先頭から始まる。そのまま追記すると先頭から
	// offset ぶんが二重になり、先頭を捨てて読むとリクエストごとに offset バイトを
	// 再転送する黙った O(n^2) になる。どちらも取らないので失敗させる。
	//
	// フィルタを併用すると mirakc は Range を黙って無視する（doc にある 400 は
	// 複数 Range のときだけ）。ingest はフィルタなしで pull するので通常は
	// 到達しないが、到達したときに黙って壊れないようここで止める。
	t.Run("offset>0 の 200 は拒否する", func(t *testing.T) {
		c := NewClient(streamRangeServer(t, content, http.StatusOK).URL, nil)
		_, _, err := c.StreamRecord(context.Background(), "rec1", 500)
		if err == nil {
			t.Fatal("offset>0 で Range を無視した 200 を成功として扱った（先頭から二重に書く）")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusOK {
			t.Fatalf("err = %v, want *APIError(200)", err)
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

func TestStreamService(t *testing.T) {
	content := strings.Repeat("T", 500)
	var gotPriority string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotPriority = r.Header.Get("X-Mirakurun-Priority")
		w.Header().Set("Content-Type", "video/MP2T")
		_, _ = fmt.Fprint(w, content)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	body, err := c.StreamService(context.Background(), 1024, 3)
	if err != nil {
		t.Fatalf("StreamService: %v", err)
	}
	defer func() { _ = body.Close() }()

	if gotPath != "/api/services/1024/stream?decode=1" {
		t.Errorf("path+query = %q, want decode=1 on the services/{id}/stream path", gotPath)
	}
	if gotPriority != "3" {
		t.Errorf("X-Mirakurun-Priority = %q, want %q", gotPriority, "3")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(data) != content {
		t.Errorf("body = %q, want %q", data, content)
	}
}

// StreamService はチューナー枯渇等の mirakc 側エラーを APIError として素通しする。
// 呼び出し側（streamer）がこれを見て 503 とメトリクス reason="upstream_error" に変換する。
func TestStreamService_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "no tuner available")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	_, err := c.StreamService(context.Background(), 1024, 3)
	if err == nil {
		t.Fatal("StreamService: want error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusServiceUnavailable)
	}
}

// ListTuners は静的な構成（index / name / types / isAvailable / isFault）だけを
// デコードし、実行時状態（users / isFree / isUsing / command / pid）は型に持たない
// （issue #21、docs/data.md §6.5）。実機のレスポンスをそのまま流して確認する。
func TestListTuners(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tuners" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
          {"index":0,"name":"PX-S1UD_T1","types":["GR"],
           "isAvailable":true,"isFault":false,
           "users":[],"isFree":true,"isUsing":false,"command":null,"pid":null},
          {"index":1,"name":"PX-W3U4_S1","types":["BS","CS"],
           "isAvailable":true,"isFault":true,
           "users":[{"agent":"epgstation","priority":1}],
           "isFree":false,"isUsing":true,"command":"recdvb","pid":4242}
        ]`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	tuners, err := c.ListTuners(context.Background())
	if err != nil {
		t.Fatalf("ListTuners: %v", err)
	}
	if len(tuners) != 2 {
		t.Fatalf("len = %d, want 2", len(tuners))
	}
	if tuners[0].Index != 0 || tuners[0].Name != "PX-S1UD_T1" {
		t.Errorf("tuners[0] = %+v, want index 0 / PX-S1UD_T1", tuners[0])
	}
	if len(tuners[0].Types) != 1 || tuners[0].Types[0] != "GR" {
		t.Errorf("tuners[0].Types = %v, want [GR]", tuners[0].Types)
	}
	if !tuners[0].IsAvailable || tuners[0].IsFault {
		t.Errorf("tuners[0] availability = (%v, %v), want (true, false)", tuners[0].IsAvailable, tuners[0].IsFault)
	}
	if !reflect.DeepEqual(tuners[1].Types, []string{"BS", "CS"}) {
		t.Errorf("tuners[1].Types = %v, want [BS CS]", tuners[1].Types)
	}
	if !tuners[1].IsFault {
		t.Errorf("tuners[1].IsFault = false, want true")
	}

	// 実行時状態は Tuner 型に載っていない。フィールドを増やすと tuner_sync へ
	// 投影する経路ができてしまうので、型の形そのものを固定する。
	fields := reflect.VisibleFields(reflect.TypeOf(Tuner{}))
	got := make([]string, 0, len(fields))
	for _, f := range fields {
		got = append(got, f.Name)
	}
	want := []string{"Index", "Name", "Types", "IsAvailable", "IsFault"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tuner fields = %v, want %v (実行時状態は持たない)", got, want)
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
		// 接続を開いたままにして、確立ログが切断後ではなくストリーム開始時に出ることを検証する。
		<-r.Context().Done()
	}))
	defer srv.Close()

	logRecords := make(chan string, 10)
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(slogRecordWriter{records: logRecords}, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

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

	var logs []string
	select {
	case line := <-logRecords:
		logs = append(logs, line)
		if !strings.Contains(line, `msg="SSE connected (stream started)"`) {
			t.Fatalf("first SSE log = %q, want connection log", line)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE connection log")
	}
	cancel()
	wg.Wait()
	for {
		select {
		case line := <-logRecords:
			logs = append(logs, line)
		default:
			goto logsCollected
		}
	}

logsCollected:
	connectedLogs := 0
	for _, line := range logs {
		if strings.Contains(line, `msg="SSE connected (stream started)"`) {
			connectedLogs++
		}
	}
	if connectedLogs != 1 {
		t.Errorf("SSE connection logs = %d, want 1; logs:\n%s", connectedLogs, strings.Join(logs, ""))
	}

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
