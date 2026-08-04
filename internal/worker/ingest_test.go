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

	fullPath := filepath.Join(mediaDir, "test", "recording.m2ts")
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

	fullPath := filepath.Join(mediaDir, "match", "recording.m2ts")
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

	fullPath := filepath.Join(mediaDir, "test", "partial.m2ts")
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

	fullPath := filepath.Join(mediaDir, "test", "stall.m2ts")
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

func insertTestRecordSync(t *testing.T, pool *pgxpool.Pool, recordingID int64, recordID string) {
	t.Helper()
	q := sqlcgen.New(pool)
	if err := q.UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
		Site:        "default",
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   327361024000001,
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

	fullPath := filepath.Join(mediaDir, "test", "reexec.m2ts")
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

	fullPath := filepath.Join(mediaDir, "test", "reingest.m2ts")
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
	insertTestRecordSync(t, pool, recordingID, "rec-policy-base")

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
	insertTestRecordSync(t, pool, recordingID, "rec-policy-override")

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
	insertTestRecordSync(t, pool, recordingID, "rec-policy-drift")

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
	insertTestRecordSync(t, pool, recordingID, "rec-policy-rematerialized")

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
	insertTestRecordSync(t, pool, recordingID, "rec-policy-rule-gone")

	// GC が想定より早く走った、または予約が恒久的に削除された場合を模す
	// （再実体化しない。TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization
	// と異なり、これが正常経路には無い異常系であることが本テストの前提）。
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
	// 00030 backfill の判定基準（原本 media_asset の有無）と issue #133 の事後追加
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
