package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
)

func jsonDecode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

// injectRecord は POST /mock/records で録画を 1 件置き、応答の録画を返す。
// 呼び出し側は 201 を前提にする。
func injectRecord(t *testing.T, base string, programID int64, body []byte) mirakc.Record {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/mock/records?programId=%d", base, programID), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /mock/records: status %d: %s", resp.StatusCode, msg)
	}
	var rec mirakc.Record
	if err := jsonDecode(resp.Body, &rec); err != nil {
		t.Fatalf("decoding injected record: %v", err)
	}
	return rec
}

// sampleBody は長さが分かり、位置ごとに違う中身の入力（Range のずれを見分けるため）。
func sampleBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestInjectedRecordIsReadThroughTheProductClient は、注入した録画を**製品のクライアント**
// （internal/mirakc.Client）で読んだとき、製品が見るフィールドが埋まり、サイズと SHA-256 が
// 中身と一致することを見る。タグが無いと製品の ProcessRecord は録画行を作らない
// （Managed の分岐）ので、タグの有無もここで固定する。
func TestInjectedRecordIsReadThroughTheProductClient(t *testing.T) {
	m, client, srv := newTestServer(t)
	ctx := context.Background()
	program := m.programs(m.now())[0]
	body := sampleBody(4096)
	sum := sha256.Sum256(body)

	injected := injectRecord(t, srv.URL, program.ID, body)

	records, err := client.ListRecords(ctx)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if !slices.ContainsFunc(records, func(r mirakc.Record) bool { return r.ID == injected.ID }) {
		t.Fatalf("ListRecords does not include %s (got %d records)", injected.ID, len(records))
	}
	got, err := client.GetRecord(ctx, injected.ID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if want := []string{mirakc.ProgramTag(program.ID)}; !slices.Equal(got.Tags, want) {
		t.Errorf("tags = %v, want %v (the watcher treats a record without the rokuban tag as not ours)", got.Tags, want)
	}
	if got.Recording.Status != "finished" {
		t.Errorf("recording.status = %q, want finished", got.Recording.Status)
	}
	if got.Program.ID != program.ID || got.Program.ServiceID != program.ServiceID || got.Program.EventID != program.EventID {
		t.Errorf("program = %+v, want the program %d", got.Program, program.ID)
	}
	if got.Service.ServiceID != program.ServiceID || got.Service.NetworkID != program.NetworkID {
		t.Errorf("service = %+v, want service %d of network %d", got.Service, program.ServiceID, program.NetworkID)
	}
	if got.Content.Length == nil || *got.Content.Length != uint64(len(body)) {
		t.Errorf("content.length = %v, want %d", got.Content.Length, len(body))
	}
	wantSum := hex.EncodeToString(sum[:])
	if got.Content.Sha256 == nil || *got.Content.Sha256 != wantSum {
		t.Errorf("content.sha256 = %v, want %s", got.Content.Sha256, wantSum)
	}
}

// TestStreamRecordHonoursRange は、製品の StreamRecord / HeadRecordStream が読む形の
// Range（`bytes=N-`）を、実 mirakc と同じ応答で返すことを見る。
//
// 壊し方の例（mutation check で実際に確かめた）: Range を無視して常に 200 で全体を返す →
// StreamRecord(offset>0) が「Range was ignored」の APIError で落ちる。
func TestStreamRecordHonoursRange(t *testing.T) {
	m, client, srv := newTestServer(t)
	ctx := context.Background()
	program := m.programs(m.now())[0]
	body := sampleBody(1000)
	rec := injectRecord(t, srv.URL, program.ID, body)

	// 製品のクライアント: offset 0 は 206 で全体。
	rc, n, err := client.StreamRecord(ctx, rec.ID, 0)
	if err != nil {
		t.Fatalf("StreamRecord(0): %v", err)
	}
	all, _ := io.ReadAll(rc)
	_ = rc.Close()
	if n != int64(len(body)) || !bytes.Equal(all, body) {
		t.Errorf("StreamRecord(0): length %d, body equal %v; want %d and equal", n, bytes.Equal(all, body), len(body))
	}

	// offset > 0 は差分だけ返す（先頭は含まない）。
	rc, n, err = client.StreamRecord(ctx, rec.ID, 300)
	if err != nil {
		t.Fatalf("StreamRecord(300): %v", err)
	}
	tail, _ := io.ReadAll(rc)
	_ = rc.Close()
	if n != 700 || !bytes.Equal(tail, body[300:]) {
		t.Errorf("StreamRecord(300): length %d, body = body[300:] %v; want 700 and equal", n, bytes.Equal(tail, body[300:]))
	}

	// 追い付いた（offset が総数）は 416 → 製品の ErrRangeNotSatisfiable。
	if _, _, err := client.StreamRecord(ctx, rec.ID, int64(len(body))); !errors.Is(err, mirakc.ErrRangeNotSatisfiable) {
		t.Errorf("StreamRecord(total) error = %v, want ErrRangeNotSatisfiable", err)
	}

	size, err := client.HeadRecordStream(ctx, rec.ID)
	if err != nil {
		t.Fatalf("HeadRecordStream: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("HeadRecordStream = %d, want %d", size, len(body))
	}

	// 生の応答の形（Content-Range の文字列まで）。製品のクライアントは Content-Range を読まないが、
	// 実 mirakc の形から外れていないことを固定する。
	raw := func(rangeHeader string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/recording/records/"+rec.ID+"/stream", nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	resp := raw("bytes=300-")
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("Range bytes=300-: status %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 300-999/1000" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 300-999/1000")
	}
	if resp := raw("bytes=1000-"); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("Range bytes=1000-: status %d, want 416", resp.StatusCode)
	}
	if resp := raw("bytes=0-10"); resp.StatusCode != http.StatusBadRequest {
		// 製品は `N-` しか送らない。未対応の形を黙って全体で返すと、
		// 製品の Range の誤りが緑になるので 400 で落とす。
		t.Errorf("Range bytes=0-10: status %d, want 400 (only bytes=N- is supported)", resp.StatusCode)
	}
	if resp := raw(""); resp.StatusCode != http.StatusOK {
		t.Errorf("no Range: status %d, want 200", resp.StatusCode)
	}
}

// TestDeleteRecordRemovesIt は、製品の ingest が取り込み後に呼ぶ DELETE（purge）が
// 録画と内容を消し、以後 GET が 404 になることを見る。
func TestDeleteRecordRemovesIt(t *testing.T) {
	m, client, srv := newTestServer(t)
	ctx := context.Background()
	rec := injectRecord(t, srv.URL, m.programs(m.now())[0].ID, sampleBody(64))

	res, err := client.DeleteRecord(ctx, rec.ID, true)
	if err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if !res.RecordRemoved || !res.ContentRemoved {
		t.Errorf("DeleteRecord = %+v, want both removed", res)
	}
	if _, err := client.GetRecord(ctx, rec.ID); err == nil {
		t.Error("GetRecord after delete succeeded, want an error (404)")
	}
}

// TestRecordSavedEventReachesTheProductClient は、注入が SSE の recording.record-saved を
// 流し、製品の購読（mirakc.Client.Subscribe）がそれを受けることを見る。
//
// 購読の登録と注入の順序は競合しうる（購読が張られる前の注入は流れない）ので、
// 受け取るまで注入を繰り返す。注入が増えても検査は「その recordId を含むイベントがある」だけ。
func TestRecordSavedEventReachesTheProductClient(t *testing.T) {
	m, client, srv := newTestServer(t)
	program := m.programs(m.now())[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan mirakc.Event, 64)
	go func() { _ = client.Subscribe(ctx, events, nil) }()

	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var injectedIDs []string
	for {
		select {
		case ev := <-events:
			if ev.Type != "recording.record-saved" {
				continue
			}
			var data mirakc.RecordSavedData
			if err := jsonDecode(bytes.NewReader(ev.Data), &data); err != nil {
				t.Fatalf("decoding event data: %v", err)
			}
			if slices.Contains(injectedIDs, data.RecordID) {
				if data.RecordingStatus != "finished" {
					t.Errorf("recordingStatus = %q, want finished", data.RecordingStatus)
				}
				return
			}
		case <-tick.C:
			rec := injectRecord(t, srv.URL, program.ID, sampleBody(16))
			injectedIDs = append(injectedIDs, rec.ID)
		case <-deadline:
			t.Fatalf("no recording.record-saved event for %v reached the product client in 10s", injectedIDs)
		}
	}
}

// TestResetClearsInjectedRecords は、周回の掃除（POST /mock/reset）が注入した録画を消すことを見る。
// 消さないと、前の周回の録画が今回の判定の根拠に残る。
func TestResetClearsInjectedRecords(t *testing.T) {
	m, client, srv := newTestServer(t)
	ctx := context.Background()
	rec := injectRecord(t, srv.URL, m.programs(m.now())[0].ID, sampleBody(16))

	resp, err := http.Post(srv.URL+"/mock/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	records, err := client.ListRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(records, func(r mirakc.Record) bool { return r.ID == rec.ID }) {
		t.Errorf("record %s survived POST /mock/reset", rec.ID)
	}
}

// TestInjectRejectsUnknownProgram は、EPG に無い programId を 404 にすることを見る。
// 201 にすると、録画行を作れない番組の録画が製品の経路に入る。
func TestInjectRejectsUnknownProgram(t *testing.T) {
	_, _, srv := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mock/records?programId=999999999", bytes.NewReader(sampleBody(16)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}
