package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
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
