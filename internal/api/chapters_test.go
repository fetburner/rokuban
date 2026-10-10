package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// editsURL は GET 用の URL から PUT / DELETE 用（chapter-edits）の URL を作る。
func editsURL(url string) string {
	return strings.TrimSuffix(url, "/chapters") + "/chapter-edits"
}

// putChapters はクライアントと同じく GET で版を取ってからタイムライン全体を PUT する。
func putChapters(t *testing.T, url string, spans []ChapterSpan) *http.Response {
	t.Helper()
	return putChaptersWithVersion(t, url, getChapters(t, url).Version, spans)
}

// putChaptersWithVersion は版を指定して PUT する（古い版の再現に使う）。
func putChaptersWithVersion(t *testing.T, url, version string, spans []ChapterSpan) *http.Response {
	t.Helper()
	resp, err := doChapters(http.MethodPut, editsURL(url), ChapterEditsInput{Version: version, Spans: spans})
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
	q := sqlcgen.New(pool)
	attemptCount := startCMDetectionAttemptForTest(t, context.Background(), q, id)
	markCMDetectionFailureForTest(t, context.Background(), q, id, attemptCount, "failed", nil, strPtr("detector unavailable"))
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

	// クライアントは検出中（結果 tx が commit される前）の版を持っている。
	pendingVersion := getChapters(t, chaptersURL(srv.URL, id)).Version

	started := make(chan struct{})
	done := make(chan struct {
		status int
		err    error
	}, 1)
	go func() {
		close(started)
		resp, err := doChapters(http.MethodPut, editsURL(chaptersURL(srv.URL, id)), ChapterEditsInput{
			Version: pendingVersion,
			Spans:   []ChapterSpan{{StartMs: 1001, EndMs: 2002, Label: strPtr("CM"), Cut: true}},
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
		// 待った後は commit された結果を見る --- 検出中の版のままなので版不一致の 409。
		if got.status != http.StatusConflict {
			t.Fatalf("PUT status after the detection commit = %d, want 409 (stale version)", got.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PUT did not return after the detection result committed")
	}

	// 空の自動層で引き取られていない。検出結果がそのまま自動層として見える。
	got := getChapters(t, chaptersURL(srv.URL, id))
	if got.Source != Auto || got.DetectionPending || len(got.Spans) != 1 || got.Spans[0].StartMs != 1001 {
		t.Fatalf("chapters = %+v, want the detected automatic layer", got)
	}
}

// TestPutRecordingChapterEdits_RejectsVersionFromBeforeDetectionCommit は、検出中に
// GET した空の層を基にした下書きが、検出 commit 後には引き取れないことを確かめる。
// 版の比較を外すと、検出が commit された後の PUT が 204 になり、検出された CM を
// 空の層で消す（このテストが落ちる形）。
func TestPutRecordingChapterEdits_RejectsVersionFromBeforeDetectionCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "古い版", time.Now().Truncate(time.Second), "finished", 990)
	enableCMDetect(t, pool, id)
	url := chaptersURL(srv.URL, id)

	pending := getChapters(t, url)
	if !pending.DetectionPending || len(pending.Spans) != 0 {
		t.Fatalf("GET while detecting = %+v, want detectionPending with no spans", pending)
	}
	draft := []ChapterSpan{{StartMs: 0, EndMs: 1001, Label: strPtr("OP"), Cut: false}}
	if resp := putChaptersWithVersion(t, url, pending.Version, draft); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT while detecting status = %d, want 409", resp.StatusCode)
	}

	saveCMDetection(t, pool, id, "{[1000,2000)}")

	resp := putChaptersWithVersion(t, url, pending.Version, draft)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT with the pre-detection version status = %d, want 409", resp.StatusCode)
	}
	var body ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding 409 body: %v", err)
	}
	if body.Error != chapterStaleVersionMessage {
		t.Fatalf("409 message = %q, want the stale-version message", body.Error)
	}
	if got := getChapters(t, url); got.Source != Auto || len(got.Spans) != 1 {
		t.Fatalf("chapters = %+v, want the detected automatic layer untouched", got)
	}

	// 再取得した版なら通る。
	if resp := putChapters(t, url, draft); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT with the fresh version status = %d, want 204", resp.StatusCode)
	}
}

// TestPutRecordingChapterEdits_OwnedRecordingIgnoresRedetection は、所有後は再検出中
// （検出結果が消えて cm_detect が真の状態）でも PUT が通ることを確かめる。所有済みは
// 自動層を読まないので検出状態は無関係。`!Owned` を条件から外すと 409 になる。
// ただし版の比較は所有済みでも効く。
func TestPutRecordingChapterEdits_OwnedRecordingIgnoresRedetection(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	id := seedRecording(t, pool, "再検出中の所有", time.Now().Truncate(time.Second), "finished", 991)
	enableCMDetect(t, pool, id)
	saveCMDetection(t, pool, id, "{[1000,2000)}")
	url := chaptersURL(srv.URL, id)

	if resp := putChapters(t, url, []ChapterSpan{{StartMs: 5005, EndMs: 6006, Cut: true}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first PUT status = %d, want 204", resp.StatusCode)
	}
	// 再検出の開始 = 結果行が消えて検出が未終端に戻る。
	if _, err := pool.Exec(context.Background(), `DELETE FROM recording_cm_detections WHERE recording_id = $1`, id); err != nil {
		t.Fatalf("clearing detection: %v", err)
	}
	got := getChapters(t, url)
	if got.Source != User || got.DetectionPending {
		t.Fatalf("GET = %+v, want source=user and no pending", got)
	}
	if resp := putChapters(t, url, []ChapterSpan{{StartMs: 7007, EndMs: 8008, Cut: true}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT on owned recording during re-detection status = %d, want 204", resp.StatusCode)
	}

	// 自動に戻して再取得するまで、古い版では通らない（版比較は所有済みでも効く）。
	stale := got.Version
	if resp, err := doChapters(http.MethodDelete, editsURL(url), nil); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: err=%v", err)
	}
	if resp := putChaptersWithVersion(t, url, stale, []ChapterSpan{{StartMs: 1001, EndMs: 2002, Cut: true}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT with a version from before the reset status = %d, want 409", resp.StatusCode)
	}
}

// TestChapterEdits_RejectTrashedRecording はごみ箱の録画が編集できないことを確かめる。
func TestChapterEdits_RejectTrashedRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "ごみ箱", time.Now().Truncate(time.Second), "finished", 992)
	url := chaptersURL(srv.URL, id)
	version := getChapters(t, url).Version
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("trashing: %v", err)
	}

	if resp := putChaptersWithVersion(t, url, version, []ChapterSpan{{StartMs: 0, EndMs: 1001, Cut: true}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT on trashed recording status = %d, want 404", resp.StatusCode)
	}
	del, err := doChapters(http.MethodDelete, editsURL(url), nil)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer func() { _ = del.Body.Close() }()
	if del.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE on trashed recording status = %d, want 404", del.StatusCode)
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

	del, err := doChapters(http.MethodDelete, editsURL(chaptersURL(srv.URL, id)), nil)
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
	again, err := doChapters(http.MethodDelete, editsURL(chaptersURL(srv.URL, id)), nil)
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

// TestPutRecordingChapterEdits_OwnedVersionChangesWithEachEdit は、所有済みの録画で
// 同じ版による 2 回目の PUT が弾かれることを確かめる（2 タブの後勝ちで前の編集が
// 黙って消えない）。版のハッシュから spans を外すと 2 回目も 204 になり落ちる。
func TestPutRecordingChapterEdits_OwnedVersionChangesWithEachEdit(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "2タブ", time.Now().Truncate(time.Second), "finished", 993)
	url := chaptersURL(srv.URL, id)
	if resp := putChapters(t, url, []ChapterSpan{{StartMs: 1001, EndMs: 2002, Cut: true}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("adopting PUT status = %d, want 204", resp.StatusCode)
	}

	v := getChapters(t, url).Version
	if resp := putChaptersWithVersion(t, url, v, []ChapterSpan{{StartMs: 3003, EndMs: 4004, Cut: true}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT with the current version status = %d, want 204", resp.StatusCode)
	}
	if resp := putChaptersWithVersion(t, url, v, []ChapterSpan{{StartMs: 5005, EndMs: 6006, Cut: true}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second PUT with the same version status = %d, want 409", resp.StatusCode)
	}
	if got := getChapters(t, url); len(got.Spans) != 1 || got.Spans[0].StartMs != 3003 {
		t.Fatalf("spans = %+v, want the first editor's spans kept", got.Spans)
	}
}
