package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func chaptersURL(base string, id int64) string {
	return fmt.Sprintf("%s/api/recordings/%d/chapters", base, id)
}

func strPtr(s string) *string { return &s }

// doChapters はボディ付きのリクエストを投げる。t.Fatal を呼ばないので
// ゴルーチンからも使える（並行テスト参照）。
func doChapters(method, url string, body any) (*http.Response, error) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// putChapters はタイムライン全体を PUT する。
func putChapters(t *testing.T, url string, spans []ChapterSpan) *http.Response {
	t.Helper()
	resp, err := doChapters(http.MethodPut, url, ChapterEditsInput{Spans: spans})
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func getChapters(t *testing.T, url string) RecordingChapters {
	t.Helper()
	resp, err := doChapters(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	var out RecordingChapters
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding GET %s: %v", url, err)
	}
	return out
}

// enableCMDetect は録画の policy 行を作り cm_detect を立てる。
func enableCMDetect(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', true)
ON CONFLICT (recording_id) DO UPDATE SET cm_detect = true`, id); err != nil {
		t.Fatalf("enabling CM detection: %v", err)
	}
}

// saveCMDetection は検出結果の行を直接書く（worker を通さないフィクスチャ）。
func saveCMDetection(t *testing.T, pool *pgxpool.Pool, id int64, ranges string) {
	t.Helper()
	if err := sqlcgen.New(pool).SaveCMDetection(context.Background(), sqlcgen.SaveCMDetectionParams{
		RecordingID: id,
		CmRanges:    ranges,
	}); err != nil {
		t.Fatalf("saving CM detection: %v", err)
	}
}

func TestGetRecordingChapters_AutoLayerReturnsCMAsCutSpans(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "自動層", time.Now().Truncate(time.Second), "finished", 981)
	enableCMDetect(t, pool, id)
	saveCMDetection(t, pool, id, "{[1000,2000)}")

	got := getChapters(t, chaptersURL(srv.URL, id))
	if got.Source != Auto {
		t.Fatalf("source = %q, want auto", got.Source)
	}
	want := []ChapterSpan{{StartMs: 1001, EndMs: 2002, Label: strPtr("CM"), Cut: true}}
	if len(got.Spans) != 1 || got.Spans[0].StartMs != want[0].StartMs ||
		got.Spans[0].EndMs != want[0].EndMs || got.Spans[0].Cut != true ||
		got.Spans[0].Label == nil || *got.Spans[0].Label != "CM" {
		t.Fatalf("spans = %+v, want %+v", got.Spans, want)
	}
}

func TestGetRecordingChapters_DropsImplausibleAutoLayer(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "壊れた検出", time.Now().Truncate(time.Second), "finished", 982)
	enableCMDetect(t, pool, id)
	// 30 分の録画の 29 分が CM という検出。壊れているので CM 無しとして返す。
	saveCMDetection(t, pool, id, "{[1000,1740000)}")

	got := getChapters(t, chaptersURL(srv.URL, id))
	if len(got.Spans) != 0 {
		t.Fatalf("spans = %+v, want none (implausible detection must be dropped)", got.Spans)
	}
	if got.Source != Auto {
		t.Fatalf("source = %q, want auto", got.Source)
	}
}

func TestPutRecordingChapterEdits_AdoptsAndIgnoresRedetection(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "引き取り", time.Now().Truncate(time.Second), "finished", 983)
	enableCMDetect(t, pool, id)
	saveCMDetection(t, pool, id, "{[1000,2000)}")

	// クライアントは GET の結果（自動層）をそのまま送る = 引き取り。
	before := getChapters(t, chaptersURL(srv.URL, id))
	resp := putChapters(t, chaptersURL(srv.URL, id), before.Spans)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}

	// 再検出がまったく違う境界を書いても、GET はユーザー層を返し続ける。
	saveCMDetection(t, pool, id, "{[12000,19000)}")

	after := getChapters(t, chaptersURL(srv.URL, id))
	if after.Source != User {
		t.Fatalf("source = %q, want user", after.Source)
	}
	if len(after.Spans) != 1 || after.Spans[0].StartMs != 1001 || after.Spans[0].EndMs != 2002 {
		t.Fatalf("spans after re-detection = %+v, want the adopted spans unchanged", after.Spans)
	}
}

func TestPutRecordingChapterEdits_RejectsWhileDetectionIsPending(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "検出中", time.Now().Truncate(time.Second), "finished", 984)
	enableCMDetect(t, pool, id)

	resp := putChapters(t, chaptersURL(srv.URL, id), []ChapterSpan{{StartMs: 0, EndMs: 1001, Cut: true}})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT while detecting status = %d, want 409", resp.StatusCode)
	}

	// 終端の失敗行があれば引き取れる（自動層は空 = CM 無しという主張）。
	if err := sqlcgen.New(pool).MarkCMDetectionRunning(context.Background(), id); err != nil {
		t.Fatalf("marking running: %v", err)
	}
	if err := sqlcgen.New(pool).MarkCMDetectionFailure(context.Background(), sqlcgen.MarkCMDetectionFailureParams{
		RecordingID: id,
		State:       "failed",
		Error:       strPtr("detector unavailable"),
	}); err != nil {
		t.Fatalf("marking failure: %v", err)
	}
	resp = putChapters(t, chaptersURL(srv.URL, id), []ChapterSpan{{StartMs: 0, EndMs: 1001, Cut: true}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT after terminal failure status = %d, want 204", resp.StatusCode)
	}
}

func TestPutRecordingChapterEdits_AllowsEditWhenDeploymentHasNoCMDetection(t *testing.T) {
	pool := testutil.SetupDB(t)
	// cm_detect.enabled=false のデプロイ。誰も検出ジョブを積まないので、終端に
	// 達しないことを理由に編集を封じない。
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "無効デプロイ", time.Now().Truncate(time.Second), "finished", 985)
	enableCMDetect(t, pool, id)

	resp := putChapters(t, chaptersURL(srv.URL, id), []ChapterSpan{{StartMs: 0, EndMs: 1001, Cut: true}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}
}

// TestPutRecordingChapterEdits_WaitsForDetectionResultTransaction は、引き取りが
// 検出結果の commit と直列化されることを確かめる。
//
// 検出結果を書く tx は recordings の行をロックしてから結果を書く。PUT も同じ行を
// 先頭でロックするので、進行中の検出がある間は待ち、commit 後に結果を見て引き取る。
// **ロックを外すと PUT は待たずに「検出中」と判断して 409 を返す**（このテストが
// 落ちる形）。
func TestPutRecordingChapterEdits_WaitsForDetectionResultTransaction(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "直列化", time.Now().Truncate(time.Second), "finished", 986)
	enableCMDetect(t, pool, id)

	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring connection: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning detection tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// 検出 worker の結果 tx と同じ順序で書く。
	if _, err := tx.Exec(ctx, `SELECT id FROM recordings WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("locking recording: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES ($1, $2::text::int8multirange)`,
		id, "{[1000,2000)}"); err != nil {
		t.Fatalf("writing CM result: %v", err)
	}

	started := make(chan struct{})
	done := make(chan struct {
		status int
		err    error
	}, 1)
	go func() {
		close(started)
		resp, err := doChapters(http.MethodPut, chaptersURL(srv.URL, id), ChapterEditsInput{
			Spans: []ChapterSpan{{StartMs: 1001, EndMs: 2002, Label: strPtr("CM"), Cut: true}},
		})
		if err != nil {
			done <- struct {
				status int
				err    error
			}{0, err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		done <- struct {
			status int
			err    error
		}{resp.StatusCode, nil}
	}()
	<-started

	select {
	case got := <-done:
		t.Fatalf("PUT returned %d while the detection result tx still held the row lock (err=%v)", got.status, got.err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing detection result: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PUT: %v", got.err)
		}
		if got.status != http.StatusNoContent {
			t.Fatalf("PUT status after the detection commit = %d, want 204", got.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PUT did not return after the detection result committed")
	}

	// 引き取ったのは空の自動層ではない --- クライアントが送った区間がそのまま残る。
	got := getChapters(t, chaptersURL(srv.URL, id))
	if got.Source != User || len(got.Spans) != 1 || got.Spans[0].StartMs != 1001 {
		t.Fatalf("chapters = %+v, want the user spans", got)
	}
}

func TestDeleteRecordingChapterEdits_ReturnsToAutoLayer(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "自動に戻す", time.Now().Truncate(time.Second), "finished", 987)
	enableCMDetect(t, pool, id)
	saveCMDetection(t, pool, id, "{[1000,2000)}")

	resp := putChapters(t, chaptersURL(srv.URL, id), []ChapterSpan{{StartMs: 5000, EndMs: 6006, Cut: true}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}

	del, err := doChapters(http.MethodDelete, chaptersURL(srv.URL, id), nil)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer func() { _ = del.Body.Close() }()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", del.StatusCode)
	}

	got := getChapters(t, chaptersURL(srv.URL, id))
	if got.Source != Auto {
		t.Fatalf("source = %q, want auto", got.Source)
	}
	if len(got.Spans) != 1 || got.Spans[0].StartMs != 1001 || got.Spans[0].EndMs != 2002 {
		t.Fatalf("spans = %+v, want the automatic layer", got.Spans)
	}

	// 冪等（既に所有していなくても 204）。
	again, err := doChapters(http.MethodDelete, chaptersURL(srv.URL, id), nil)
	if err != nil {
		t.Fatalf("DELETE again: %v", err)
	}
	defer func() { _ = again.Body.Close() }()
	if again.StatusCode != http.StatusNoContent {
		t.Fatalf("second DELETE status = %d, want 204", again.StatusCode)
	}
}

func TestPutRecordingChapterEdits_RejectsInvalidSpans(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "不正な区間", time.Now().Truncate(time.Second), "finished", 988)
	url := chaptersURL(srv.URL, id)

	cases := []struct {
		name  string
		spans []ChapterSpan
	}{
		{"empty span", []ChapterSpan{{StartMs: 2002, EndMs: 2002, Cut: true}}},
		{"overlap", []ChapterSpan{
			{StartMs: 0, EndMs: 2002, Cut: true},
			{StartMs: 1001, EndMs: 3003, Cut: true},
		}},
		{"neither label nor cut", []ChapterSpan{{StartMs: 0, EndMs: 2002}}},
	}
	for _, c := range cases {
		resp := putChapters(t, url, c.spans)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %s status = %d, want 400", c.name, resp.StatusCode)
		}
	}

	// 空のタイムラインは有効な主張（CM もチャプターも無い）。
	resp := putChapters(t, url, []ChapterSpan{})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT empty timeline status = %d, want 204", resp.StatusCode)
	}
}
