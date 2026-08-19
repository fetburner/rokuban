package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// makeTSData は指定バイト数の 188 バイト境界に揃った TS データを生成する。
//
// **添字の純関数である**: パケット i の中身は packets（総数）に依存しない
// ので、makeTSData(30) は makeTSData(50) のバイト単位の前置になる。2 つの
// 転送を区別可能なバイト列で比較したいテストは makeTSData ではなく
// makeTSDataFill を使う（issue #281 のレビュー指摘: 長さ比較に退化した
// バイト比較が rename を commit 前に移す変異を見逃していた）。
func makeTSData(packets int) []byte {
	data := make([]byte, packets*188)
	for i := 0; i < packets; i++ {
		off := i * 188
		data[off] = 0x47
		data[off+1] = 0x01
		data[off+2] = 0x00
		data[off+3] = 0x10 | byte(i%16)
	}
	return data
}

// makeTSDataFill は makeTSData と同じ TS パケットヘッダ（sync byte 0x47 等、
// tsstat が読む先頭 4 バイト）を持ちつつ、ペイロード（各パケットの残り 184
// バイト）を fill で塗りつぶした TS データを生成する。fill には 0x10〜0x1F
// （ヘッダ 4 バイト目が取りうる範囲）と衝突しない値を渡すこと。
//
// 2 本の並行 ingest を区別可能なバイト列で比較するために使う ---
// makeTSData(30) は makeTSData(50) のバイト単位の前置なので、長さの異なる
// makeTSData だけでは「先行ファイルが後発のバイトで上書きされていないか」を
// 全バイト比較しても実は前置一致で通ってしまう変異を見逃す。
func makeTSDataFill(packets int, fill byte) []byte {
	data := makeTSData(packets)
	for i := 0; i < packets; i++ {
		off := i * 188
		for j := off + 4; j < off+188; j++ {
			data[j] = fill
		}
	}
	return data
}

func TestIngestWorker_FullTransfer(t *testing.T) {
	tsData := makeTSData(100)

	var deleteRequested atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/recording.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/recording.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			deleteRequested.Store(true)
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-001")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-001"},
	}

	err := w.Work(context.Background(), job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "recording.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}

	if !deleteRequested.Load() {
		t.Error("edge record was not deleted after successful commit")
	}
}

// TestIngestWorker_SiteMismatch は、args.Site がワーカー自身の site
// （w.Site、config.mirakc.site 相当）と一致しないジョブを、mirakc に一切
// 触れずに fail-fast することを確認する（issue #139）。
//
// モックは何を投げても 200 を返す --- 「args.Site を無視する実装でも mirakc が
// 404 を返せば落ちる」形の弱いテストにしないため（issue #139 のテスト規律）。
// record_sync / recordings は「site-b」側で実際に存在する体で用意する
// （lookupRecordingID がここで成功してしまうと、ガードを外したときに
// GetRecord まで進んで初めてモックへ到達する、という現実の壊れ方を再現できる）。
func TestIngestWorker_SiteMismatch(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(makeTSData(1))
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: strPtr("mismatch/recording.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/mismatch/recording.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "site-a",
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	// site-b 側では実在する record_sync / recordings（site-b の watcher が
	// 実際に作った行という想定）。
	recordingID := insertTestRecordingForSite(t, pool, "site-b", 20240101)
	insertTestRecordSyncForSite(t, pool, "site-b", recordingID, "rec-mismatch", 327361024000099)

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "site-b", RecordID: "rec-mismatch"},
	}

	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", got, err)
	}
}

// TestIngestWorker_SiteMatch は、args.Site がワーカー自身の site と一致する
// ジョブは従来どおり処理されることを確認する（TestIngestWorker_SiteMismatch と
// 対になる、両方向の確認。CLAUDE.md テスト規律「分岐を直したら両方向で確認する」）。
func TestIngestWorker_SiteMatch(t *testing.T) {
	tsData := makeTSData(20)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: strPtr("match/recording.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/match/recording.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "site-a",
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecordingForSite(t, pool, "site-a", 20240102)
	insertTestRecordSyncForSite(t, pool, "site-a", recordingID, "rec-match", 327361024000098)

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "site-a", RecordID: "rec-match"},
	}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}

	// rel_path には args.Site（ここでは "site-a"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "site-a", "match", "recording.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}
}

func TestIngestWorker_MidTransferDisconnect(t *testing.T) {
	tsData := makeTSData(100)
	cutoff := 50 * 188

	var attempt atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeHeader := r.Header.Get("Range")
			var offset int
			if rangeHeader != "" {
				_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &offset)
			}

			if attempt.Add(1) == 1 && offset == 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tsData[:cutoff])
				return
			}

			remaining := tsData[offset:]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
			if offset > 0 {
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			_, _ = w.Write(remaining)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/partial.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/partial.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-partial")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-partial"},
	}

	err := w.Work(context.Background(), job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "partial.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}

	if attempt.Load() < 2 {
		t.Errorf("expected at least 2 stream attempts, got %d", attempt.Load())
	}
}

func TestIngestWorker_SizeMismatch(t *testing.T) {
	tsData := makeTSData(50)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			// HEAD が異なるサイズを返す → size mismatch
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)+1000))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/mismatch.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/mismatch.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-mismatch")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-mismatch"},
	}

	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("expected error for size mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("expected 'size mismatch' error, got: %v", err)
	}
}

func TestIngestWorker_StallDetection(t *testing.T) {
	tsData := makeTSData(10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeHeader := r.Header.Get("Range")
			var offset int
			if rangeHeader != "" {
				_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &offset)
			}

			if offset == 0 {
				flusher, ok := w.(http.Flusher)
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tsData[:188])
				if ok {
					flusher.Flush()
				}
				// stall: don't send more data, wait for context cancellation
				<-r.Context().Done()
				return
			}

			remaining := tsData[offset:]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(remaining)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/stall.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/stall.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 200 * time.Millisecond,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-stall")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-stall"},
	}

	err := w.Work(context.Background(), job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "stall.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}
}

func strPtr(s string) *string { return &s }

// setupTestPool はマイグレーション済みのテスト用プールを返す。
func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testutil.SetupDB(t)
}

func insertTestRecording(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	q := sqlcgen.New(pool)
	id, err := q.CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           1,
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		ProgramStartAt:    time.Now(),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("inserting test recording: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM drop_stats WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1)", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM media_assets WHERE recording_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM record_sync WHERE recording_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM recordings WHERE id = $1", id)
	})
	return id
}

// noProgramRecordSyncProgramID は insertTestRecordSync が使う固定の program_id。
// 呼び出し側は insertTestRecording（program_snapshots を作らない）で録画だけを
// 用意していることを前提にしており、この値は意図的にどの program_snapshots
// 行とも対応しない。テストが扱っている番組（insertProgramSnapshotAndReservation /
// insertTestRecordingForReservation に渡した programID）と record_sync を
// 対応させたい場合は insertTestRecordSyncForSite を使うこと（対応させないまま
// insertTestRecordSync を使うと、record_sync.program_id を読む将来の実装に対する
// テストが黙って空虚な PASS になる）。
const noProgramRecordSyncProgramID = 327361024000001

// insertTestRecordSync は番組と対応しない record_sync 行を 1 件作る
// （noProgramRecordSyncProgramID 参照）。
func insertTestRecordSync(t *testing.T, pool *pgxpool.Pool, recordingID int64, recordID string) {
	t.Helper()
	q := sqlcgen.New(pool)
	if err := q.UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
		Site:        "default",
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   noProgramRecordSyncProgramID,
		Status:      "finished",
		Tags:        []string{},
	}); err != nil {
		t.Fatalf("inserting test record_sync: %v", err)
	}
}

// insertTestRecordingForSite は insertTestRecording の site 指定版。
// TestIngestWorker_SiteMismatch / TestIngestWorker_SiteMatch のように、
// ワーカー自身の site と異なる（あるいは異なる想定の）site の下で recordings
// 行が実在する状況を再現するために使う。eventID は他のテストの固定値
// （event_id=1）と衝突しないよう呼び出し側で変えてもらう。
func insertTestRecordingForSite(t *testing.T, pool *pgxpool.Pool, site string, eventID int32) int64 {
	t.Helper()
	q := sqlcgen.New(pool)
	id, err := q.CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              site,
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           eventID,
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		ProgramStartAt:    time.Now(),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("inserting test recording (site=%s): %v", site, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM drop_stats WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1)", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM media_assets WHERE recording_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM record_sync WHERE recording_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM recordings WHERE id = $1", id)
	})
	return id
}

// insertTestRecordSyncForSite は insertTestRecordSync の site / programID 指定版
// （insertTestRecordingForSite と対で使う）。
func insertTestRecordSyncForSite(t *testing.T, pool *pgxpool.Pool, site string, recordingID int64, recordID string, programID int64) {
	t.Helper()
	q := sqlcgen.New(pool)
	if err := q.UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
		Site:        site,
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   programID,
		Status:      "finished",
		Tags:        []string{},
	}); err != nil {
		t.Fatalf("inserting test record_sync (site=%s): %v", site, err)
	}
}

// TestRecordSyncFixture_ProgramIDMatchesRecordingBroadcastEvent は、番組を
// 扱っているテスト（insertProgramSnapshotAndReservation +
// insertTestRecordingForReservation）が record_sync を作るとき、
// insertTestRecordSyncForSite で渡した programID が実際にその録画と同じ放送
// イベントを指す program_snapshots 行を指していることを確認する。
//
// insertTestRecordSync（program_id を固定値でハードコードする汎用ヘルパ）を
// ここで使うと、record_sync.program_id はこのテストの programID とは無関係な
// 値になり、record_sync → program_snapshots の JOIN がこの録画の放送イベント
// キーとは異なる行（あるいは存在しない行）を指す。それを検出する。
func TestRecordSyncFixture_ProgramIDMatchesRecordingBroadcastEvent(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()

	programID := int64(900000000285001)
	insertProgramSnapshotAndReservation(t, pool, programID, "フィクスチャ確認番組")
	recordingID := insertTestRecordingForReservation(t, pool, programID)
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-fixture-check", programID)

	var recNetworkID, recServiceID, recEventID int32
	if err := pool.QueryRow(ctx,
		"SELECT network_id, service_id, event_id FROM recordings WHERE id = $1", recordingID,
	).Scan(&recNetworkID, &recServiceID, &recEventID); err != nil {
		t.Fatalf("querying recording broadcast event key: %v", err)
	}

	var snapNetworkID, snapServiceID, snapEventID int32
	if err := pool.QueryRow(ctx,
		`SELECT ps.network_id, ps.service_id, ps.event_id
		 FROM record_sync rs
		 JOIN program_snapshots ps ON ps.site = rs.site AND ps.program_id = rs.program_id
		 WHERE rs.recording_id = $1`, recordingID,
	).Scan(&snapNetworkID, &snapServiceID, &snapEventID); err != nil {
		t.Fatalf("querying record_sync's program_snapshots via program_id: %v", err)
	}

	if recNetworkID != snapNetworkID || recServiceID != snapServiceID || recEventID != snapEventID {
		t.Errorf("record_sync.program_id resolves to broadcast event (%d,%d,%d), want the recording's own (%d,%d,%d)",
			snapNetworkID, snapServiceID, snapEventID, recNetworkID, recServiceID, recEventID)
	}
}

func TestIngestWorker_JobReexecution(t *testing.T) {
	tsData := makeTSData(50)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/reexec.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/reexec.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-reexec")

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "reexec.m2ts")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("this is leftover data from a crashed previous attempt that is longer than the real file")
	if err := os.WriteFile(fullPath, garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	w := &IngestWorker{
		MirakcClient: mc,
		Pool:         pool,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-reexec"},
	}

	err := w.Work(context.Background(), job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d (should truncate previous attempt)", len(data), len(tsData))
	}
	if string(data[:4]) != string(tsData[:4]) {
		t.Error("file content does not match expected TS data (truncate may have failed)")
	}
}

// TestIngestWorker_SkipsTransferWhenAlreadyCommitted は、エッジ record の削除
// （DeleteRecord）が失敗して mirakc 側に record が残り、同じジョブ引数で
// Work が再実行された（record_sweep 経由の再投入を模している）場合の挙動を
// 確認する。修正前は 2 回目の Work が os.Create でコミット済みファイルを
// 0 バイトに切り詰めて全量を再転送し、最後に media_assets の unique 制約
// 違反でエラーになっていた。
func TestIngestWorker_SkipsTransferWhenAlreadyCommitted(t *testing.T) {
	tsData := makeTSData(50)

	var streamRequests atomic.Int32
	var deleteAttempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			streamRequests.Add(1)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/reingest.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/reingest.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			attempt := deleteAttempts.Add(1)
			if attempt == 1 {
				// エッジ record の削除が最初に失敗するのを再現する。
				// ingest 自体はコミット済みなのでこの失敗はログのみで success 扱い
				// （既存の意図的な挙動）だが、mirakc 側には record が残る。
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-reingest")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-reingest"},
	}

	// 1 回目: 転送・コミットは成功するが、DeleteRecord は失敗する。
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "reingest.m2ts")
	firstData, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file after first Work(): %v", err)
	}
	if len(firstData) != len(tsData) {
		t.Fatalf("file size after first Work() = %d, want %d", len(firstData), len(tsData))
	}

	// record_sweep が「まだ mirakc に record がある」ことを検知して同じジョブ引数を
	// 再投入したのと同じ状況を、同じ job で Work をもう一度呼ぶことで再現する。
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("second Work() error: %v", err) // (a)
	}

	secondData, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file after second Work(): %v", err)
	}
	if len(secondData) != len(tsData) {
		// (c) ファイルが切り詰められていない
		t.Errorf("file size after second Work() = %d, want %d (file must not be truncated)",
			len(secondData), len(tsData))
	}
	if !bytes.Equal(secondData, firstData) {
		t.Error("file content changed after second Work() (transfer should have been skipped)")
	}

	if got := streamRequests.Load(); got != 1 {
		t.Errorf("stream requests = %d, want 1 (second Work() must skip the transfer entirely)", got)
	}

	var mediaAssetCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
	).Scan(&mediaAssetCount); err != nil {
		t.Fatalf("counting media_assets: %v", err)
	}
	if mediaAssetCount != 1 {
		// (b) media_assets が 1 行のまま
		t.Errorf("media_assets rows = %d, want 1", mediaAssetCount)
	}

	if got := deleteAttempts.Load(); got != 2 {
		// (d) 2 回目でも DeleteRecord が再試行されている
		t.Errorf("delete attempts = %d, want 2 (edge record deletion must be retried)", got)
	}
}

// TestStallReader はストールリーダーのタイマーリセット動作をテストする。
func TestStallReader(t *testing.T) {
	data := []byte("hello world")
	r := strings.NewReader(string(data))
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	sr := &stallReader{r: r, timer: timer, d: 100 * time.Millisecond}

	buf := make([]byte, 5)
	n, err := sr.Read(buf)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if n != 5 {
		t.Errorf("Read() = %d, want 5", n)
	}

	n, err = sr.Read(buf)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if n != 5 {
		t.Errorf("Read() = %d, want 5", n)
	}

	n, err = sr.Read(buf)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if n != 1 {
		t.Errorf("Read() = %d, want 1", n)
	}

	n, err = sr.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want EOF", err)
	}
	if n != 0 {
		t.Errorf("Read() = %d, want 0", n)
	}
}

// --- M3-14 (issue #103): encode policy (keep_original / encode_profiles) の
// スナップショット。resolveAndSnapshotEncodePolicy（internal/worker/ingest.go）が
// ingest コミット tx 内で recordings に焼くことをエンドツーエンドで確認する。

// newFullTransferServer は「1 回で完走する」ingest 用のテストサーバーを返す。
// 以下の 3 テストで共通の mirakc 差し替え。
func newFullTransferServer(t *testing.T, tsData []byte, contentPath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr(contentPath)},
				},
				Content: mirakc.ContentInfo{Path: "/recording/" + contentPath},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// insertProgramSnapshotAndReservation は programID の program_snapshots 行と、
// base の無い手動予約を作る。base だけは setReservationBase で後から模す
// （reservations の実運用上の書き手は ruler の 1 パス UPSERT だけで、
// internal/ruler 内の unexported 関数はテストから直接呼べないため）。
func insertProgramSnapshotAndReservation(t *testing.T, pool *pgxpool.Pool, programID int64, title string) sqlcgen.Reservation {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        "default",
		ProgramID:   programID,
		Title:       title,
		StartAt:     time.Now(),
		DurationMs:  1800000,
		NetworkID:   32736,
		ServiceID:   1024,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     int32(programID % 100000),
		ServiceName: "テスト局",
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	res, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      "default",
		ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_snapshots WHERE site = $1 AND program_id = $2", "default", programID)
	})
	return res
}

// setReservationBase は ruler が書く reservations.base を模したテスト用フィクスチャ。
// internal/worker/encode_test.go が recordings.encode_profiles を raw SQL で
// 直接作るのと同じ規律（reservations.sql は #52 並走中につき、この目的のためだけの
// 書き込みクエリを新設しない）。
func setReservationBase(t *testing.T, pool *pgxpool.Pool, reservationID int64, base string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE reservations SET base = $2 WHERE id = $1", reservationID, base); err != nil {
		t.Fatalf("setting reservation base: %v", err)
	}
}

// insertTestRecordingForReservation は放送イベントキーで予約にリンクする
// 録画行を作る。recordings.reservation_id という結合キー自体は issue #158 で
// 落としたので、呼び出し側が渡す programID の event_id を一致させることが
// リンクの唯一の手段になる。
//
// programID は insertProgramSnapshotAndReservation に渡したものと同じ値を渡す。
// resolveAndSnapshotEncodePolicy（issue #149 以降）は放送イベントキー
// (site, network_id, service_id, event_id) で予約を引くので、この録画の
// event_id は program_snapshots 側の event_id（insertProgramSnapshotAndReservation
// が programID % 100000 から作る）と一致させる必要がある --- ここが一致しなければ
// 「予約が引けなかった」側に落ちて、想定した予約とは無関係な結果になる。
func insertTestRecordingForReservation(t *testing.T, pool *pgxpool.Pool, programID int64) int64 {
	t.Helper()
	q := sqlcgen.New(pool)
	id, err := q.CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "rule",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           int32(programID % 100000), // program_snapshots の event_id と一致させる
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		ProgramStartAt:    time.Now(),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("inserting test recording: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM drop_stats WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1)", id)
		_, _ = pool.Exec(ctx, "DELETE FROM media_assets WHERE recording_id = $1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM record_sync WHERE recording_id = $1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM recordings WHERE id = $1", id)
	})
	return id
}

// riverWorkContext は resolveAndSnapshotEncodePolicy の後段（enqueueMissingEncodesFromContext /
// EnqueueThumbnailIfNeeded）が実際にジョブを投入するよう、Work() 実行中と同じ
// river.Client をコンテキストに載せる。river.ClientFromContextSafely はジョブ実行中の
// コンテキストからしか取れないため、素の context.Background() だとヒント投入が
// 静かにスキップされ、「encode ジョブが投入される」を確認できない。
func riverWorkContext(t *testing.T, pool *pgxpool.Pool) context.Context {
	t.Helper()
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	return rivertest.WorkContext(context.Background(), client)
}

// encodePolicyOfRecording は recording_encode_policy 衛星表（issue #159）を読む。
// 行が無い（未凍結）場合は既定値 "always" / [] を返す --- resolveAndSnapshotEncodePolicy
// が解決に失敗した場合や、予約が無く一度も呼ばれない場合はこの表に行自体が
// 作られない（不変条件 10「意味を持たない行を作らない」）。既存テストの
// アサーションはいずれも「凍結されなかった＝既定値のまま」を確認する意図なので、
// この関数がその意図をそのまま表す。
func encodePolicyOfRecording(t *testing.T, pool *pgxpool.Pool, recordingID int64) (keepOriginal string, profiles []string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		"SELECT keep_original, encode_profiles FROM recording_encode_policy WHERE recording_id = $1", recordingID,
	).Scan(&keepOriginal, &profiles)
	if err == nil {
		return keepOriginal, profiles
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "always", []string{}
	}
	t.Fatalf("querying recording encode policy: %v", err)
	return "", nil
}

// encodePolicyRowExists は recording_encode_policy に行そのものがあるかを見る。
// encodePolicyOfRecording は ErrNoRows を既定値に潰すため「行がある + 既定値」と
// 「行が無い」を区別できない --- resolveAndSnapshotEncodePolicy が解決に失敗
// しても凍結する（issue #159 の中心的な設計判断。doc コメント「解決に失敗しても
// 凍結する」参照）ことを確認するテストは、この関数で行の有無そのものを見る
// 必要がある。
func encodePolicyRowExists(t *testing.T, pool *pgxpool.Pool, recordingID int64) bool {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1", recordingID,
	).Scan(&count); err != nil {
		t.Fatalf("counting recording encode policy rows: %v", err)
	}
	return count == 1
}

func countEncodeJobs(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job
		 WHERE kind = 'encode'
		   AND (args->>'recording_id')::bigint = $1
		   AND args->>'profile' = $2`,
		recordingID, profile,
	).Scan(&count); err != nil {
		t.Fatalf("counting encode jobs: %v", err)
	}
	return count
}

// TestIngestWorker_SnapshotsEncodePolicyFromRuleBase は issue #103 の受け入れ基準
// 「ルールに encodeProfiles を設定して録画 → ingest 完了後に
// recordings.encode_profiles が一致し、encode ジョブが投入される」を確認する。
func TestIngestWorker_SnapshotsEncodePolicyFromRuleBase(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	programID := int64(900000000000001)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "ルール予約番組")
	setReservationBase(t, pool, res.ID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	// program_id をこのテストが扱っている programID に一致させる
	// （insertTestRecordSync は固定値をハードコードしており対応しない）。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-base", programID)

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-base.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-base"},
	}

	ctx := riverWorkContext(t, pool)
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded", keepOriginal)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265]", profiles)
	}

	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs for h265 = %d, want 1", got)
	}
}

// TestIngestWorker_SnapshotsEncodePolicyFromOverride は「PATCH .../overrides で
// encodeProfiles を上書きした予約が、ingest 後の recordings にその値で焼かれる」を
// 確認する。base（ルール由来: h264 / always）と overrides（ユーザー上書き: h265 /
// until_encoded）が競合するとき、db.EffectiveOptions を通して overrides が勝つ
// ことも合わせて確認する。
func TestIngestWorker_SnapshotsEncodePolicyFromOverride(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000002)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "上書き予約番組")
	setReservationBase(t, pool, res.ID, `{"keepOriginal":"always","encodeProfiles":["h264"]}`)

	overrides, err := json.Marshal(map[string]any{
		"keepOriginal":   "until_encoded",
		"encodeProfiles": []string{"h265"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:      "default",
		ProgramID: programID,
		Overrides: overrides,
	}); err != nil {
		t.Fatalf("setting override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	// program_id をこのテストが扱っている programID に一致させる
	// （insertTestRecordSync は固定値をハードコードしており対応しない）。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-override", programID)

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-override.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-override"},
	}

	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded (override should win over base's always)", keepOriginal)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265] (override should win over base's [h264])", profiles)
	}

	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs for h265 = %d, want 1", got)
	}
	if got := countEncodeJobs(t, pool, recordingID, "h264"); got != 0 {
		t.Errorf("encode jobs for h264 = %d, want 0 (base profile must be fully replaced by override)", got)
	}
}

// TestIngestWorker_ClampsUntilEncodedWithEmptyProfiles は issue #104 との相互作用の
// 回帰テスト: override が keepOriginal=until_encoded だけを立て、encodeProfiles は
// （ルール側の h264 を明示的に打ち消して）空にした場合、実効値としては
// until_encoded × 空プロファイルというドリフトが生成される。この組み合わせは
// recordings.keep_original / encode_profiles への CHECK 制約
// （cardinality(encode_profiles) > 0、issue #104）に違反するため、そのまま書くと
// 原本 media_asset の INSERT と同一 tx がロールバックし録画が消失する
// （不変条件 3「コミット = DB 行」）。resolveAndSnapshotEncodePolicy のクランプで
// keepOriginal を 'always' に倒して書くことを確認する。
func TestIngestWorker_ClampsUntilEncodedWithEmptyProfiles(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000003)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "ドリフト予約番組")
	setReservationBase(t, pool, res.ID, `{"keepOriginal":"always","encodeProfiles":["h264"]}`)

	overrides, err := json.Marshal(map[string]any{
		"keepOriginal":   "until_encoded",
		"encodeProfiles": []string{}, // ルールの h264 を明示的に打ち消す override
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:      "default",
		ProgramID: programID,
		Overrides: overrides,
	}); err != nil {
		t.Fatalf("setting override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	// program_id をこのテストが扱っている programID に一致させる
	// （insertTestRecordSync は固定値をハードコードしており対応しない）。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-drift", programID)

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-drift.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-drift"},
	}

	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always (clamped: effective encodeProfiles is empty)", keepOriginal)
	}
	if len(profiles) != 0 {
		t.Errorf("encode_profiles = %v, want empty", profiles)
	}

	var jobCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1`,
		recordingID,
	).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 0 {
		t.Errorf("encode jobs = %d, want 0", jobCount)
	}
}

// TestIngestWorker_NoReservation_LeavesEncodePolicyDefault は「予約行が無い録画では
// 既定値のまま・encode ジョブも入らない」を確認する（手動で mirakc に起こされた
// 録画等、放送イベントキーで program_snapshots → reservations を引いても
// 何も見つからないケース。insertTestRecording が作る録画にはそもそも対応する
// program_snapshots / reservations 行が無い）。
func TestIngestWorker_NoReservation_LeavesEncodePolicyDefault(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-policy-none")

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-none.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-none"},
	}

	ctx := riverWorkContext(t, pool)
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always (default, unchanged)", keepOriginal)
	}
	if len(profiles) != 0 {
		t.Errorf("encode_profiles = %v, want empty (default, unchanged)", profiles)
	}

	var jobCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1`,
		recordingID,
	).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 0 {
		t.Errorf("encode jobs = %d, want 0", jobCount)
	}
}

// TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults は issue #214 の決定
// 「encode 意図が生き残る滞留の上限は epg.retention_grace であって、エッジの
// リングバッファの N 日ではない」を固定する。
//
// エッジのリングバッファは「回線断・クラウド側障害で未 ingest の record が
// N 日分溜まる」ことを前提にサイジングする（docs/operations.md §4）が、凍結の
// JOIN 先 program_snapshots の寿命は放送終了 + epg.retention_grace（既定 24h）で
// 決まる（docs/storage.md §6）。**2 つの時計の間に制約が無い**ので、猶予を
// 超えた滞留から復帰した ingest は予約を引けず、既定値で凍結される。
//
// このテストは「そうなる」ことを固定する ---
// TestIngestWorker_NoReservation_LeavesEncodePolicyDefault が「予約が最初から
// 無い」を模すのに対し、こちらは**予約と意図が確かに存在したうえで GC に刈られた**
// 経路を、実際の GC クエリ（DeleteEndedProgramSnapshots）を通して模す。
//
// **record_sync の program_id を programID に一致させるのが要点。** GC を未 ingest の
// record_sync と連動させる案（issue #214 の案 1）は、record_sync が持つ唯一の
// 番組キー (site, program_id) でスナップショットを引いて留め置く形になる。
// 汎用ヘルパ insertTestRecordSync は program_id を固定値でハードコードしている
// ので、それを使うとこの record_sync 行は GC 対象のスナップショットを指さず、
// **案 1 を実装してもこのテストは通り続ける**（実際に通ってしまっていた。
// PR #270 のレビューで発覚）。programID を渡せる insertTestRecordSyncForSite を
// 使い、案 1 の鍵付けを実際に成立させる。
//
// 案 1 を (site, program_id) で実装すると、このテストは
// 「DeleteEndedProgramSnapshots deleted 0 rows, want 1」で落ちる（確認済み）。
// 決定を変えるならこのテストも一緒に変える。
func TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000010)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "滞留番組")
	setReservationBase(t, pool, res.ID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	// program_id を programID に一致させる（doc コメント参照）。status='finished' /
	// 原本 media_asset なしなので、案 1 から見て「未 ingest の record」に該当する。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-gced", programID)

	// 放送は 48 時間前に終わったことにする（insertProgramSnapshotAndReservation は
	// start_at = now() で作る）。GC の cutoff は epg.retention_grace = 24h 相当。
	if _, err := pool.Exec(ctx,
		"UPDATE program_snapshots SET start_at = now() - interval '48 hours' WHERE site = $1 AND program_id = $2",
		"default", programID,
	); err != nil {
		t.Fatalf("aging program snapshot: %v", err)
	}

	// ruler の GC 本体（internal/ruler.runGC が呼ぶのと同じクエリ）。FK CASCADE で
	// reservations / program_intents / program_overrides も一緒に落ちる。
	deleted, err := q.DeleteEndedProgramSnapshots(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("running GC: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteEndedProgramSnapshots deleted %d rows, want 1 "+
			"（未 ingest の record_sync（program_id=%d）があるスナップショットも刈るのが issue #214 の決定。"+
			"GC を record_sync と連動させる案 1 を実装したならここで落ちる —— 決定を変えるならこのテストも変える）",
			deleted, programID)
	}
	var reservations int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2", "default", programID,
	).Scan(&reservations); err != nil {
		t.Fatalf("counting reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("reservations after GC = %d, want 0 (FK CASCADE で一緒に落ちるはず)", reservations)
	}

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-gced.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-gced"},
	}

	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// 凍結そのものはスキップされない（issue #159）。行はあり、値が既定値になる。
	if !encodePolicyRowExists(t, pool, recordingID) {
		t.Error("recording_encode_policy に行が無い。解決に失敗しても既定値で凍結する契約（issue #159）が破れている")
	}
	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always "+
			"（猶予超過の滞留では予約を引けないので既定値で凍結される。issue #214）", keepOriginal)
	}
	if len(profiles) != 0 {
		t.Errorf("encode_profiles = %v, want empty （予約の h265 は GC で失われている。issue #214）", profiles)
	}
	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 0 {
		t.Errorf("encode jobs for h265 = %d, want 0 "+
			"（原本は keep_original='always' で残るがエンコードは投入されない。issue #214）", got)
	}
}

// TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization は
// issue #149 の受け入れ基準「予約の再実体化を跨いでもエンコード方針が焼き込まれる」
// を確認する。
//
// ruler は EPG フリッカー・ルール編集・dedup で予約を導出削除・再実体化し、
// reservations.id が変わる（#53 / #98 / #99 と同じ族。CLAUDE.md 不変条件 9
// 「identity」の 5 例目）。旧実装は録画開始時に watcher が焼いた
// recordings.reservation_id（当時 FK の ON DELETE SET NULL。issue #158 で
// 列自体を削除済み）を宛先に GetReservationEncodePolicy を引いていたため、
// 再実体化で NULL に落ちて「予約が無い」と誤認し、encode policy を凍結し
// 損なっていた。
//
// internal/reconciler/never_scheduled_identity_test.go の
// TestReconciler_NeverScheduledExclusionSurvivesRematerialization と同じ模し方
// （DELETE してから CreateManualReservation で作り直す。id が変わることも
// 明示的に確認する）。
func TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000004)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "再実体化予約番組")
	setReservationBase(t, pool, res.ID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	// watcher.createRecording が録画開始時に放送イベントキーを焼いた状態を模す。
	recordingID := insertTestRecordingForReservation(t, pool, programID)
	// program_id をこのテストが扱っている programID に一致させる
	// （insertTestRecordSync は固定値をハードコードしており対応しない）。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-rematerialized", programID)

	// ruler の導出削除 → 再実体化を模す（同じ番組・新しい id）。旧実装は
	// recordings.reservation_id（当時 FK の ON DELETE SET NULL。issue #158 で
	// 列自体を削除済み）を宛先にしていたため、この DELETE で NULL に落ちて
	// 「予約が無い」と誤認する穴があった。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE id = $1`, res.ID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	res2, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("re-materializing reservation: %v", err)
	}
	if res2.ID == res.ID {
		t.Fatalf("再実体化で id が変わっていない（テストの前提が崩れている）")
	}
	// ruler は再実体化のたびに射影から base を書き直す。テストではその 1 パスを
	// 模して同じ base を新しい行に立て直す（setReservationBase 参照）。
	setReservationBase(t, pool, res2.ID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-rematerialized.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-rematerialized"},
	}

	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded (予約の再実体化を跨いで解決できているはず)", keepOriginal)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265] (予約の再実体化を跨いで解決できているはず)", profiles)
	}

	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs for h265 = %d, want 1", got)
	}
}

// TestIngestWorker_LogsWarnWhenRuleSourceReservationUnresolvable は
// source='rule'（DeriveRecordingSource が「作成時点で予約があり、intent
// action='record' の行は無かった」ことを保証する）の録画で放送イベントキーの
// JOIN が失敗する異常系を再現し、slog.Warn に識別子が残ることを確認する
// （レビュー指摘: internal/worker/ingest.go:539-547 の「引けなかった」を黙って
// return nil にしない、の rule 側）。
func TestIngestWorker_LogsWarnWhenRuleSourceReservationUnresolvable(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()

	programID := int64(900000000000005)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "恒久削除予約番組")
	recordingID := insertTestRecordingForReservation(t, pool, programID)
	// program_id をこのテストが扱っている programID に一致させる
	// （insertTestRecordSync は固定値をハードコードしており対応しない）。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-rule-gone", programID)

	// 予約が恒久的に削除された（または GC が想定より早く走った）場合を模す
	// （再実体化しない。TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization
	// と異なり、これが正常経路には無い異常系であることが本テストの前提）。
	// JOIN 失敗の 3 つ目の原因（GC は設計どおり走ったが ingest が猶予を跨いで
	// 遅れた。issue #214）は TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults
	// が別に持つ —— そちらは異常系ではなく設計が許容するシナリオ。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE id = $1`, res.ID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-rule-gone.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-rule-gone"},
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	logText := logBuf.String()
	if !strings.Contains(logText, "level=WARN") {
		t.Errorf("expected a WARN log for source=rule reservation unresolvable, got:\n%s", logText)
	}
	if !strings.Contains(logText, "reservation not found via broadcast event key") {
		t.Errorf("expected log message to mention the unresolved lookup, got:\n%s", logText)
	}
	for _, want := range []string{
		fmt.Sprintf("recording_id=%d", recordingID),
		"network_id=32736",
		"service_id=1024",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("expected log to contain identifier %q, got:\n%s", want, logText)
		}
	}

	// 解決に失敗しても凍結自体はスキップしない（issue #159。resolveAndSnapshotEncodePolicy
	// の doc コメント「解決に失敗しても凍結する」参照）ので既定値で凍結されるはず
	// （この関数の主張の対象ではないが、「ログは出たが desired が誤って書かれた」を
	// 排除するために確認する）。
	//
	// encodePolicyRowExists で行そのものの有無を確認する --- encodePolicyOfRecording
	// は ErrNoRows を既定値に潰すため、旧実装（ErrNoRows で return nil して凍結を
	// スキップする）に戻しても値の比較だけでは検出できない。行が無いと migration
	// 00032 backfill の判定基準（原本 media_asset の有無）と issue #133 の事後追加
	// （AppendRecordingEncodeProfiles が「行が既にある」前提で書けること）が破れる。
	if !encodePolicyRowExists(t, pool, recordingID) {
		t.Fatalf("recording_encode_policy row missing for recording %d; resolveAndSnapshotEncodePolicy must freeze defaults even when the lookup fails", recordingID)
	}
	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" || len(profiles) != 0 {
		t.Errorf("keep_original/encode_profiles = %q/%v, want always/[] (unresolved, frozen to defaults)", keepOriginal, profiles)
	}
}

// TestIngestWorker_LogsInfoWhenManualSourceReservationUnresolvable は
// source='manual' の録画で JOIN が失敗した場合でも「引けなかった」が必ず
// ログに残ることを確認する（レビュー指摘: DeriveRecordingSource
// (internal/db/recording_source.go) は intent action='record' があれば予約の
// 有無に関わらず 'manual' を返すため、rec.Source == db.SourceRule だけを見る
// 判定では「ユーザーが手動予約して encodeProfiles を指定した録画」で解決に
// 失敗したときだけログが一切出ず、issue #149 が問題にした症状がそのまま
// 残っていた）。
//
// このテストは「予約がそもそも存在しない日常的な manual 録画」
// （insertTestRecording と同じ形。TestIngestWorker_NoReservation_LeavesEncodePolicyDefault
// が既に確認している）を使って再現する —— DeriveRecordingSource は
// intent 由来の 'manual' と予約皆無の 'manual' を区別できないので、
// このケースでもログが出ることが「manual を判定軸にしない」ことの直接的な
// 証拠になる。修正前の実装（rec.Source == db.SourceRule のときだけ警告）では
// このテストは一切ログが出ずに落ちる。
func TestIngestWorker_LogsInfoWhenManualSourceReservationUnresolvable(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-policy-manual-unresolved")

	tsData := makeTSData(20)
	srv := newFullTransferServer(t, tsData, "test/policy-manual-unresolved.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-policy-manual-unresolved"},
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	workCtx := riverWorkContext(t, pool)
	if err := w.Work(workCtx, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	logText := logBuf.String()
	if !strings.Contains(logText, "level=INFO") {
		t.Errorf("expected an INFO log for source=manual reservation unresolvable, got:\n%s", logText)
	}
	if !strings.Contains(logText, "reservation not found via broadcast event key") {
		t.Errorf("expected log message to mention the unresolved lookup, got:\n%s", logText)
	}
	if !strings.Contains(logText, fmt.Sprintf("recording_id=%d", recordingID)) {
		t.Errorf("expected log to contain recording_id=%d, got:\n%s", recordingID, logText)
	}
	if strings.Contains(logText, "level=WARN") {
		t.Errorf("source=manual unresolved reservation must not be logged at WARN (mixes the daily case with the anomalous one), got:\n%s", logText)
	}

	// source='rule' の対応テストと同じ理由で、行の有無そのものを確認する
	// （encodePolicyOfRecording は ErrNoRows を既定値に潰すため使えない）。
	if !encodePolicyRowExists(t, pool, recordingID) {
		t.Fatalf("recording_encode_policy row missing for recording %d; resolveAndSnapshotEncodePolicy must freeze defaults even when the lookup fails", recordingID)
	}
	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" || len(profiles) != 0 {
		t.Errorf("keep_original/encode_profiles = %q/%v, want always/[] (unresolved, frozen to defaults)", keepOriginal, profiles)
	}
}

// mirakcRecordServer は determineRelPath 系のテスト用に、固定の contentPath /
// content.Path を返す mirakc モックを立てる。ストリーム・削除エンドポイントは
// 常に成功する（このファイル内の他テストと同じ最小構成）。
func mirakcRecordServer(t *testing.T, tsData []byte, contentPath *string, contentFilePath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: contentPath}},
				Content:   mirakc.ContentInfo{Path: contentFilePath},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestIngestWorker_RelPathPrefixedWithSite は、issue #186 (M4-14) 受け入れの
// 1 項目目を固定する: site "tokyo" の worker が ingest した原本の
// media_assets.rel_path が "sites/tokyo/" で始まり、実ファイルがその下に置かれる
// （contentPath に階層があってもその下に入る）。
//
// determineRelPath の前置行（"relPath = "sites/" + args.Site + "/" + relPath"）を
// 削って前置なしに戻すと、rel_path が "20240101/prog.m2ts" のままになりこの
// テストは失敗する（アサーション失敗。ビルドは通る）。
func TestIngestWorker_RelPathPrefixedWithSite(t *testing.T) {
	tsData := makeTSData(10)
	srv := mirakcRecordServer(t, tsData, strPtr("20240101/prog.m2ts"), "/recording/20240101/prog.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "tokyo",
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecordingForSite(t, pool, "tokyo", 101)
	insertTestRecordSyncForSite(t, pool, "tokyo", recordingID, "rec-tokyo", 327361024000101)

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "tokyo", RecordID: "rec-tokyo"},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	q := sqlcgen.New(pool)
	asset, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset: %v", err)
	}
	const wantRelPath = "sites/tokyo/20240101/prog.m2ts"
	if asset.RelPath != wantRelPath {
		t.Errorf("media_assets.rel_path = %q, want %q (site prefix missing)", asset.RelPath, wantRelPath)
	}

	fullPath := filepath.Join(mediaDir, "sites", "tokyo", "20240101", "prog.m2ts")
	if _, err := os.Stat(fullPath); err != nil {
		t.Errorf("expected file at %s (site-prefixed dir): %v", fullPath, err)
	}
}

// TestIngestWorker_RelPathPrefixedWithSite_FallbackContentPath は受け入れの
// 2 項目目を固定する: mirakc の contentPath が空で filepath.Base(record.Content.Path)
// にフォールバックする経路でも "sites/{site}/" が前置される。
func TestIngestWorker_RelPathPrefixedWithSite_FallbackContentPath(t *testing.T) {
	tsData := makeTSData(10)
	// ContentPath を nil にしてフォールバック経路を通す。
	srv := mirakcRecordServer(t, tsData, nil, "/recording/fallback/plain.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "tokyo",
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecordingForSite(t, pool, "tokyo", 102)
	insertTestRecordSyncForSite(t, pool, "tokyo", recordingID, "rec-tokyo-fallback", 327361024000102)

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "tokyo", RecordID: "rec-tokyo-fallback"},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	q := sqlcgen.New(pool)
	asset, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset: %v", err)
	}
	// フォールバックは filepath.Base なので階層は失われ "plain.m2ts" だけが残る。
	// そこに "sites/tokyo/" が前置される。
	const wantRelPath = "sites/tokyo/plain.m2ts"
	if asset.RelPath != wantRelPath {
		t.Errorf("media_assets.rel_path = %q, want %q (site prefix missing on fallback path)", asset.RelPath, wantRelPath)
	}
}

// TestIngestWorker_DegenerateContentPath_Rejected は、mirakc の contentPath /
// Content.Path がどちらも空という縮退したレスポンスを、前置前に明示的に拒否する
// ことを固定する。
//
// 前置なしの実装なら relPath = filepath.Base("") = "." が mediapath.Resolve に
// そのまま渡り "path escapes the media directory" で弾かれていたが、前置後は
// "sites/{site}/." が Join/Clean で "sites/{site}" という一見正当なパスになって
// 素通りしてしまう（PR #196 の追レビューで発見）。determineRelPath の
// `if relPath == "." || relPath == "/"` のガードを削るとこのテストが失敗する
// （"Work() error = nil, want non-nil" というアサーション失敗。ビルドは通る）。
func TestIngestWorker_DegenerateContentPath_Rejected(t *testing.T) {
	tsData := makeTSData(10)
	// ContentPath も Content.Path も空。
	srv := mirakcRecordServer(t, tsData, nil, "")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClient: mc,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "tokyo",
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Pool = pool

	recordingID := insertTestRecordingForSite(t, pool, "tokyo", 103)
	insertTestRecordSyncForSite(t, pool, "tokyo", recordingID, "rec-tokyo-degenerate", 327361024000103)

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "tokyo", RecordID: "rec-tokyo-degenerate"},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("Work() error = nil, want non-nil for a record with no usable content path")
	}

	// 副作用（sites/tokyo をファイルとして作る等）が残っていないことを確認する。
	sitesDir := filepath.Join(mediaDir, "sites")
	entries, statErr := os.ReadDir(sitesDir)
	if statErr == nil {
		for _, e := range entries {
			if e.Name() == "tokyo" && !e.IsDir() {
				t.Errorf("sites/tokyo was created as a regular file (would break all future ingests for this site): %+v", e)
			}
		}
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("reading %s: %v", sitesDir, statErr)
	}
}

// TestIngestWorker_TwoSitesSameContentPath_DoNotCollide は受け入れの 3 項目目
// （罠の核心）を固定する: 同じ contentPath を持つ 2 サイトの record を ingest
// しても、rel_path の site 前置で実ファイルが別々になり両方が commit できる。
//
// 反転確認: determineRelPath の前置（"relPath = "sites/" + args.Site + "/" +
// relPath"）を削ると、2 回目の Work() が media_assets の一意索引違反
// （CREATE UNIQUE INDEX ON media_assets (rel_path) WHERE state <> 'deleted'）で
// 失敗し、このテストは "second Work() error" で落ちる。実際に前置行を削って
// 確認済み（PR 本文参照）。
func TestIngestWorker_TwoSitesSameContentPath_DoNotCollide(t *testing.T) {
	tsDataA := makeTSData(10)
	tsDataB := makeTSData(20)
	const sharedContentPath = "shared/recording.m2ts"

	srvA := mirakcRecordServer(t, tsDataA, strPtr(sharedContentPath), "/recording/"+sharedContentPath)
	srvB := mirakcRecordServer(t, tsDataB, strPtr(sharedContentPath), "/recording/"+sharedContentPath)

	mediaDir := t.TempDir()

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingIDA := insertTestRecordingForSite(t, pool, "site-a", 201)
	insertTestRecordSyncForSite(t, pool, "site-a", recordingIDA, "rec-collide-a", 327361024000201)
	recordingIDB := insertTestRecordingForSite(t, pool, "site-b", 202)
	insertTestRecordSyncForSite(t, pool, "site-b", recordingIDB, "rec-collide-b", 327361024000202)

	wA := &IngestWorker{
		MirakcClient: mirakc.NewClient(srvA.URL, nil),
		Pool:         pool,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "site-a",
	}
	wB := &IngestWorker{
		MirakcClient: mirakc.NewClient(srvB.URL, nil),
		Pool:         pool,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
		Site:         "site-b",
	}

	jobA := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "site-a", RecordID: "rec-collide-a"},
	}
	jobB := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "site-b", RecordID: "rec-collide-b"},
	}

	if err := wA.Work(context.Background(), jobA); err != nil {
		t.Fatalf("first Work() (site-a) error: %v", err)
	}
	if err := wB.Work(context.Background(), jobB); err != nil {
		t.Fatalf("second Work() (site-b) error: %v (site prefix likely missing, colliding on rel_path %q)", err, sharedContentPath)
	}

	pathA := filepath.Join(mediaDir, "sites", "site-a", "shared", "recording.m2ts")
	pathB := filepath.Join(mediaDir, "sites", "site-b", "shared", "recording.m2ts")

	dataA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("reading site-a output file: %v", err)
	}
	if len(dataA) != len(tsDataA) {
		t.Errorf("site-a file size = %d, want %d", len(dataA), len(tsDataA))
	}

	dataB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("reading site-b output file: %v", err)
	}
	if len(dataB) != len(tsDataB) {
		t.Errorf("site-b file size = %d, want %d", len(dataB), len(tsDataB))
	}

	q := sqlcgen.New(pool)
	assetA, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingIDA)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset(site-a): %v", err)
	}
	assetB, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingIDB)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset(site-b): %v", err)
	}
	if assetA.RelPath == assetB.RelPath {
		t.Errorf("both sites committed the same rel_path %q; site prefix must make them distinct", assetA.RelPath)
	}
	if assetA.RelPath != "sites/site-a/"+sharedContentPath {
		t.Errorf("site-a rel_path = %q, want %q", assetA.RelPath, "sites/site-a/"+sharedContentPath)
	}
	if assetB.RelPath != "sites/site-b/"+sharedContentPath {
		t.Errorf("site-b rel_path = %q, want %q", assetB.RelPath, "sites/site-b/"+sharedContentPath)
	}
}

// --- issue #197: os.Create が宛先ファイルを truncate してから一意性を知る
// （既存の active な media_asset を壊して 23505 で落ちる）の回帰テスト。

// TestIngestWorker_RelPathConflict_RefusesWithoutCorruptingExistingFile は
// issue #197 の受け入れ基準 1 項目目を固定する: 同じ rel_path になる 2 つ目の
// ingest が、先行の実ファイルを壊さずに失敗する。
//
// checkRelPathConflict の述語は `state <> 'deleted'`（一意索引の述語と同じ）
// であり、'active' と 'deleting' の両方を「衝突」として塞ぐ。これを
// `state = 'active'` に狭める変異（'deleting' を見逃す）は worker パッケージ
// 全緑のまま通ってしまう穴だった（PR #267 のレビューで指摘・実測）。
// 'deleting' 行のファイルは delete_reconcile の 3 段階
// （active → deleting → unlink → deleted、delete_reconcile.go の
// deleteMediaAsset）のうち unlink 前・unlink 失敗中はまだ実在し、かつ
// resolveUnqualifiedDeletingAsset がファイルの現存を確認した上でその行を
// active に戻しうる（issue #105）。したがって 'deleting' 行のファイルを
// ingest が上書きすると、それが後から active として復元され、issue #197
// が問題にした「DB は active、実体は別番組」がここでも再生産される。
// そのためテーブル駆動で 'active' / 'deleting' の両方を確認する
// （CLAUDE.md テスト規律「分岐を直したら両方向で確認する」）。
func TestIngestWorker_RelPathConflict_RefusesWithoutCorruptingExistingFile(t *testing.T) {
	for _, existingState := range []string{"active", "deleting"} {
		t.Run(existingState, func(t *testing.T) {
			testRelPathConflictRefusesWithoutCorruptingExistingFile(t, existingState)
		})
	}
}

// testRelPathConflictRefusesWithoutCorruptingExistingFile は
// TestIngestWorker_RelPathConflict_RefusesWithoutCorruptingExistingFile の
// 本体。existingState は先行の media_asset に与える state（"active" /
// "deleting"）。
//
// 修正前（determineRelPath の直後の事前チェックを外す）は、2 つ目の
// Work() が os.Create で先行の実ファイル（21 バイトの「正しい」中身）を
// 0 バイトに truncate し、新しい TS（tsDataNew）で上書きしてから
// media_assets の一意索引違反（23505）でようやく失敗する --- つまり
// エラーは返るが、その時点で先行ファイルは既に壊れている。「両方失敗する」
// だけでは検知できないため、このテストは失敗後に**先行ファイルの中身を
// 実際に読んで**元のバイト列のままであることを確認する。
func testRelPathConflictRefusesWithoutCorruptingExistingFile(t *testing.T, existingState string) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()

	// 先行: 既に existingState の media_asset として commit 済みの録画
	// （別の recording_id）。実ファイルは既存の「正しい」中身を持つ。
	existingRecordingID := insertTestRecordingForSite(t, pool, "default", 301)
	const conflictRelPath = "sites/default/shared/conflict.m2ts"
	q := sqlcgen.New(pool)
	existingAssetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: existingRecordingID,
		Kind:        "original",
		RelPath:     conflictRelPath,
		SizeBytes:   21,
	})
	if err != nil {
		t.Fatalf("creating existing media_asset: %v", err)
	}
	if existingState != "active" {
		// MarkMediaAssetDeleting（delete_reconcile.go）は deleted_at を
		// 立てない --- 'deleting' は unlink 前後の中間状態であり、不可逆な
		// 削除確定（deleted_at）はまだ起きていない。ここでもその形を模す。
		if _, err := pool.Exec(ctx,
			"UPDATE media_assets SET state = $2 WHERE id = $1", existingAssetID, existingState,
		); err != nil {
			t.Fatalf("setting existing media_asset state to %s: %v", existingState, err)
		}
	}

	existingContent := []byte("existing-untouched-01") // 21 バイト
	if len(existingContent) != 21 {
		t.Fatalf("test fixture bug: existingContent must be 21 bytes, got %d", len(existingContent))
	}
	conflictFullPath := filepath.Join(mediaDir, "sites", "default", "shared", "conflict.m2ts")
	if err := os.MkdirAll(filepath.Dir(conflictFullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflictFullPath, existingContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// 新規: 同じ contentPath（→ 同じ site 前置後の rel_path）を持つ別の録画。
	newRecordingID := insertTestRecordingForSite(t, pool, "default", 302)
	insertTestRecordSyncForSite(t, pool, "default", newRecordingID, "rec-conflict-new", 327361024000302)

	tsDataNew := makeTSData(20)
	var streamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			streamRequests.Add(1)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsDataNew)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsDataNew)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsDataNew)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: strPtr("shared/conflict.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/shared/conflict.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	w := &IngestWorker{
		MirakcClient: mirakc.NewClient(srv.URL, nil),
		Pool:         pool,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-conflict-new"},
	}

	err = w.Work(ctx, job)
	if err == nil {
		t.Fatal("Work() error = nil, want an explicit failure for a conflicting rel_path")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("Work() error = %v, want it to mention refusing to overwrite (distinct from a bare 23505 unique-violation message)", err)
	}

	// 核心のアサーション: 先行の実ファイルは中身まで元のままである
	// （サイズが同じだけでは truncate → 途中まで書いた場合を見逃す）。
	gotContent, err := os.ReadFile(conflictFullPath)
	if err != nil {
		t.Fatalf("reading pre-existing file after failed Work(): %v", err)
	}
	if !bytes.Equal(gotContent, existingContent) {
		t.Errorf("pre-existing file was modified: got %q, want unchanged %q", gotContent, existingContent)
	}

	// 転送そのものが始まっていないこと（「開始してから失敗」ではなく
	// 「開始前に失敗」であることの確認）。
	if got := streamRequests.Load(); got != 0 {
		t.Errorf("stream requests = %d, want 0 (transfer must not start when rel_path already conflicts)", got)
	}

	// 新規録画側は media_asset がコミットされていない。
	var newAssetCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", newRecordingID,
	).Scan(&newAssetCount); err != nil {
		t.Fatalf("counting media_assets for new recording: %v", err)
	}
	if newAssetCount != 0 {
		t.Errorf("media_assets rows for new recording = %d, want 0 (must not commit on conflict)", newAssetCount)
	}

	// 先行録画側の行は変わらず 1 のまま。
	var existingAssetCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", existingRecordingID,
	).Scan(&existingAssetCount); err != nil {
		t.Fatalf("counting media_assets for existing recording: %v", err)
	}
	if existingAssetCount != 1 {
		t.Errorf("media_assets rows for existing recording = %d, want 1 (untouched)", existingAssetCount)
	}
}

// TestIngestWorker_RelPathConflict_AllowsReuseAfterDeleted は issue #197 の
// 受け入れ基準 2 項目目（罠: state <> 'deleted' の条件を落とさない）を固定する:
// 同じ rel_path を持つ既存行が state='deleted'（tombstone）の場合は、その
// rel_path を新しい ingest が正当に再利用できる --- 既存の削除済み行を
// 「active な衝突」として誤って弾いてはいけない。
//
// checkRelPathConflict の `AND state <> 'deleted'` を落とす（例えば無条件で
// rel_path 一致を見る）と、このテストは "Work() error" で失敗する
// （tombstone を衝突と誤認して転送を始めずに落ちる）。
func TestIngestWorker_RelPathConflict_AllowsReuseAfterDeleted(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()

	// 先行: 同じ rel_path を使っていたが、既に削除済み（tombstone）の行。
	deletedRecordingID := insertTestRecordingForSite(t, pool, "default", 311)
	const reusedRelPath = "sites/default/shared/reused.m2ts"
	q := sqlcgen.New(pool)
	deletedAssetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: deletedRecordingID,
		Kind:        "original",
		RelPath:     reusedRelPath,
		SizeBytes:   9,
	})
	if err != nil {
		t.Fatalf("creating tombstoned media_asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", deletedAssetID,
	); err != nil {
		t.Fatalf("marking media_asset deleted: %v", err)
	}

	// 新規: 同じ rel_path になる録画。
	newRecordingID := insertTestRecordingForSite(t, pool, "default", 312)
	insertTestRecordSyncForSite(t, pool, "default", newRecordingID, "rec-reuse-new", 327361024000312)

	tsData := makeTSData(15)
	srv := mirakcRecordServer(t, tsData, strPtr("shared/reused.m2ts"), "/recording/shared/reused.m2ts")

	w := &IngestWorker{
		MirakcClient: mirakc.NewClient(srv.URL, nil),
		Pool:         pool,
		MediaDir:     mediaDir,
		StallTimeout: 5 * time.Second,
	}

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-reuse-new"},
	}

	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("Work() error = %v, want nil (a tombstoned rel_path must be reusable)", err)
	}

	fullPath := filepath.Join(mediaDir, "sites", "default", "shared", "reused.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}

	asset, err := q.GetActiveOriginalMediaAsset(ctx, newRecordingID)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset: %v", err)
	}
	if asset.RelPath != reusedRelPath {
		t.Errorf("new media_asset rel_path = %q, want %q", asset.RelPath, reusedRelPath)
	}
}

// --- issue #281: コミット前の転送が宛先パスを in-place で触る（#197 の残余
// TOCTOU）の回帰テスト群。方針は「宛先へ直接書く現行の形（os.Create）を維持
// したまま、転送を始める前に rel_path の Postgres advisory lock を取る」
// （一時ファイル・rename・予約行は導入しない。docs/recording/ingest.md §5.3）。

// TestIngestWorker_ConcurrentSameRelPath_LoserNeverOpensStream は、同じ
// contentPath を持つ 2 つの recording を並行実行したとき、advisory lock を
// 取れなかった側（B）が mirakc のストリームを一度も開かずに失敗し、先に
// ロックを取った側（A）の宛先ファイルが A 自身のバイトのまま（B に触られて
// いない）ことを固定する。
//
// A と B には makeTSDataFill で区別可能なペイロード（互いの前置にならない）を
// 与える --- makeTSData は添字の純関数なので、無地の makeTSData 同士だと
// 短い方が長い方のバイト前置になり、「後発が先行ファイルを上書きしていないか」
// の検証が長さ比較に退化してしまう（PR #338 のレビュー指摘）。
//
// 壊し方（コンパイルは通る）:
//  1. acquireRelPathLock の呼び出しを os.Create の後ろへ移す
//     → B が /records/{B}/stream を叩いてしまい、bStreamRequests のアサーションで落ちる
//  2. acquired を無視して常に続行する（if !acquired { ... } を消す）
//     → 同じく B がストリームを開いてしまい、アサーションで落ちる
func TestIngestWorker_ConcurrentSameRelPath_LoserNeverOpensStream(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()

	const relContentPath = "shared/race.m2ts"
	// A と B は**同じパケット数**（同じ長さ）にする。長さを変えると
	// 「後発が先行ファイルを上書きしていないか」の検証が長さ比較だけでも
	// たまたま通ってしまう（PR #338 のレビュー指摘の逆側）。長さを揃えた上で
	// fill だけを変えることで、バイト内容の比較そのものが load-bearing に
	// なる --- 何らかの理由で B が最後まで書き切ってしまう変異が起きても、
	// 長さは A と一致したまま中身だけが違う状態になり、bytes.Equal による
	// 全バイト比較でなければ検出できない。
	tsDataA := makeTSDataFill(30, 0xAA)
	tsDataB := makeTSDataFill(30, 0xBB)

	reachedMidTransfer := make(chan struct{})
	releaseTransfer := make(chan struct{})
	var aStreamRequests atomic.Int64

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			aStreamRequests.Add(1)
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsDataA)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsDataA[:188]) // 1 パケットだけ先に流し、B を試す間だけ止める
			if flusher != nil {
				flusher.Flush()
			}
			close(reachedMidTransfer)
			<-releaseTransfer
			_, _ = w.Write(tsDataA[188:])

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsDataA)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: strPtr(relContentPath)}},
				Content:   mirakc.ContentInfo{Path: "/recording/" + relContentPath},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	// release より先に登録すると、A の t.Fatalf が release の前に走ったとき
	// srvA.Close() がブロック中のハンドラを待ってデッドロックする
	// （PR #338 のレビューで 120 秒超のハングとして観測された）。t.Cleanup は
	// LIFO なので、後で登録する release 系のクリーンアップが先に実行される。
	t.Cleanup(srvA.Close)

	var bStreamRequests atomic.Int64
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			bStreamRequests.Add(1)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsDataB)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsDataB)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsDataB)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: strPtr(relContentPath)}},
				Content:   mirakc.ContentInfo{Path: "/recording/" + relContentPath},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srvB.Close)

	recA := insertTestRecordingForSite(t, pool, "default", 501)
	insertTestRecordSyncForSite(t, pool, "default", recA, "rec-race-a", 327361024000501)
	recB := insertTestRecordingForSite(t, pool, "default", 502)
	insertTestRecordSyncForSite(t, pool, "default", recB, "rec-race-b", 327361024000502)

	wA := &IngestWorker{MirakcClient: mirakc.NewClient(srvA.URL, nil), Pool: pool, MediaDir: mediaDir, StallTimeout: 5 * time.Second}
	wB := &IngestWorker{MirakcClient: mirakc.NewClient(srvB.URL, nil), Pool: pool, MediaDir: mediaDir, StallTimeout: 5 * time.Second}

	jobA := &river.Job[IngestJobArgs]{JobRow: &rivertype.JobRow{}, Args: IngestJobArgs{Site: "default", RecordID: "rec-race-a"}}
	jobB := &river.Job[IngestJobArgs]{JobRow: &rivertype.JobRow{}, Args: IngestJobArgs{Site: "default", RecordID: "rec-race-b"}}

	errACh := make(chan error, 1)
	go func() { errACh <- wA.Work(context.Background(), jobA) }()

	// release は t.Cleanup で登録する（どの t.Fatalf よりも実行上先に走る。
	// 上の t.Cleanup(srvA.Close) より後に登録することで LIFO の実行順を保証する）。
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTransfer) }) }
	t.Cleanup(release)

	<-reachedMidTransfer // A がロックを取得しストリームを開いている（B はまだ試していない）

	errB := wB.Work(context.Background(), jobB)
	if errB == nil {
		t.Fatal("Work() (B) error = nil, want an error (rel_path is locked by A's in-flight transfer)")
	}
	if got := bStreamRequests.Load(); got != 0 {
		t.Errorf("B's mirakc stream requests = %d, want 0 (B must lose at the lock, before ever opening the stream)", got)
	}

	release()
	errA := <-errACh
	if errA != nil {
		t.Fatalf("Work() (A) error: %v", errA)
	}

	fullPath := filepath.Join(mediaDir, "sites", "default", relContentPath)
	gotData, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading committed file: %v", err)
	}
	if !bytes.Equal(gotData, tsDataA) {
		t.Errorf("committed file content is not exactly A's bytes (len got=%d want=%d)", len(gotData), len(tsDataA))
	}
	if bytes.Contains(gotData, []byte{0xBB}) {
		t.Error("committed file contains B's fill byte; B must never have written to the destination")
	}

	var bAssetCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", recB,
	).Scan(&bAssetCount); err != nil {
		t.Fatalf("counting media_assets for B: %v", err)
	}
	if bAssetCount != 0 {
		t.Errorf("media_assets rows for B = %d, want 0 (B must not have committed)", bAssetCount)
	}
}

// TestIngestWorker_ReleasesRelPathLockAfterCommit は、ingest 成功後に
// rel_path の advisory lock が解放されていることを固定する。ingest が使った
// のとは別の独立したプール（別セッション）から同じキーの
// pg_try_advisory_lock が true を返せば、解放されている証拠になる。
//
// 壊し方: acquireRelPathLock の release から pg_advisory_unlock の呼び出しを
// 消し conn.Release() だけ残す → ロックが保持されたまま残り、検証用の
// 独立プールから pg_try_advisory_lock が false を返すため
// "rel_path advisory lock still held" で落ちる（確認済み。もし通ってしまう
// なら pgxpool が Release でセッション状態を暗黙に捨てているということなので、
// その事実を PR 本文に書く）。
func TestIngestWorker_ReleasesRelPathLockAfterCommit(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	tsData := makeTSData(10)
	srv := newFullTransferServer(t, tsData, "test/lock-release.m2ts")
	mc := mirakc.NewClient(srv.URL, nil)

	mediaDir := t.TempDir()
	w := &IngestWorker{MirakcClient: mc, Pool: pool, MediaDir: mediaDir, StallTimeout: 5 * time.Second}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-lock-release")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-lock-release"},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	const relPath = "sites/default/test/lock-release.m2ts"
	key := relPathLockKey(relPath)

	// ingest が使ったのとは独立したプール（新しい物理コネクション・新しい
	// セッション）で確認する --- 同じプールを再利用すると、たまたま同じ
	// コネクションが再利用された場合に「同じセッションが自分の持つロックを
	// 再度要求する」形になり得るため、解放の有無を正しく判定できない。
	//
	// **接続先は pool.Config() から複製する（testutil.DatabaseURL(t) の生 URL
	// ではない）。** advisory lock は Postgres では database スコープ
	// （pg_locks.database で見える）なので、testutil.DatabaseURL(t) が指す
	// ベース DB（ROKUBAN_TEST_DATABASE_URL そのもの）と、setupTestPool が
	// 実際に使うパッケージ専用 DB（testutil.ensurePackageTestDatabase が
	// 導出する別データベース）は別物 --- 生 URL に繋ぐと常に無関係な
	// database の advisory lock 名前空間を見ることになり、release の有無に
	// 関わらず常に「解放されている」という偽陰性を返す（このテストを書く
	// 過程で実際に踏んだ: 一度目はこのミスで壊れた実装でも見た目上パスした）。
	verifyPool, err := pgxpool.NewWithConfig(context.Background(), pool.Config())
	if err != nil {
		t.Fatalf("creating verification pool: %v", err)
	}
	defer verifyPool.Close()

	var stillFree bool
	if err := verifyPool.QueryRow(context.Background(), "SELECT pg_try_advisory_lock($1)", key).Scan(&stillFree); err != nil {
		t.Fatalf("checking rel_path lock free: %v", err)
	}
	if !stillFree {
		t.Fatal("rel_path advisory lock still held after ingest completed (release did not call pg_advisory_unlock)")
	}
	if _, err := verifyPool.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key); err != nil {
		t.Fatalf("releasing verification lock: %v", err)
	}
}
