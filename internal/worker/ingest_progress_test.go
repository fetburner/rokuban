package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/mirakc"
)

// ingestProgressRow は recording_ingest_progress の 1 行（無ければ ok=false）。
type ingestProgressRow struct {
	written  int64
	expected *int64
}

func readIngestProgress(t *testing.T, pool *pgxpool.Pool, recordingID int64) (ingestProgressRow, bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT written_bytes, expected_bytes FROM recording_ingest_progress WHERE recording_id = $1`,
		recordingID)
	if err != nil {
		t.Fatalf("querying recording_ingest_progress: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ingestProgressRow{}, false
	}
	var row ingestProgressRow
	if err := rows.Scan(&row.written, &row.expected); err != nil {
		t.Fatalf("scanning recording_ingest_progress: %v", err)
	}
	return row, true
}

// setRecordSyncContentLength は watcher が観測した content_length を立てる
// （進捗の分母）。
func setRecordSyncContentLength(t *testing.T, pool *pgxpool.Pool, recordID string, length int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE record_sync SET content_length = $1 WHERE site = 'default' AND record_id = $2`,
		length, recordID); err != nil {
		t.Fatalf("setting record_sync.content_length: %v", err)
	}
}

// TestIngestWorker_ProgressVisibleDuringTransfer は、転送の途中で
// recording_ingest_progress に進捗が見えること、そしてコミット時にその行が
// 消えることを確認する（issue #212）。
//
// mirakc スタブは半分だけ書いて flush し、テストが進捗行を観測するまで残りを
// 送らない --- 「転送が終わってから 1 回だけ書く」実装ではここで観測できない
// （そして観測できないことこそが issue #212 の症状そのもの）。
func TestIngestWorker_ProgressVisibleDuringTransfer(t *testing.T) {
	tsData := makeTSData(1000) // 188 KB
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			half := len(tsData) / 2
			_, _ = w.Write(tsData[:half])
			w.(http.Flusher).Flush()
			<-release
			_, _ = w.Write(tsData[half:])

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/progress.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/progress.m2ts"},
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

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	w := &IngestWorker{
		MirakcClient: mirakc.NewClient(srv.URL, nil),
		MediaDir:     t.TempDir(),
		StallTimeout: 30 * time.Second,
		// 既定の 2 秒だと「転送中に見えるか」を測るためにテストが 2 秒待つ
		// ことになる。間隔そのものは本題ではないので短くする。
		ProgressInterval: time.Millisecond,
		Pool:             pool,
	}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-progress")
	setRecordSyncContentLength(t, pool, "rec-progress", int64(len(tsData)))

	done := make(chan error, 1)
	go func() {
		done <- w.Work(context.Background(), &river.Job[IngestJobArgs]{
			JobRow: &rivertype.JobRow{},
			Args:   IngestJobArgs{Site: "default", RecordID: "rec-progress"},
		})
	}()

	// 転送の途中で「1 バイト以上書けた」が見えるまで待つ。
	var mid ingestProgressRow
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, ok := readIngestProgress(t, pool, recordingID)
		if ok && row.written > 0 {
			mid = row
			break
		}
		if time.Now().After(deadline) {
			close(release)
			<-done
			t.Fatalf("no ingest progress observed during transfer (row=%+v ok=%v)", row, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if mid.written >= int64(len(tsData)) {
		t.Errorf("mid-transfer written_bytes = %d, want less than the full %d (transfer was not observed in flight)",
			mid.written, len(tsData))
	}
	// 分母は record_sync.content_length のコピー（HEAD でもファイル stat でもない）。
	if mid.expected == nil || *mid.expected != int64(len(tsData)) {
		t.Errorf("mid-transfer expected_bytes = %v, want %d", mid.expected, len(tsData))
	}

	// コミット前は原本 media_asset がまだ無い（コミット = DB 行。不変条件 3）。
	var assets int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'original'`,
		recordingID).Scan(&assets); err != nil {
		t.Fatalf("counting media_assets: %v", err)
	}
	if assets != 0 {
		t.Errorf("media_assets during transfer = %d, want 0", assets)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// コミットの tx が進捗行を消す --- 「原本があるのに取り込み中」を読者に
	// 見せない。
	if row, ok := readIngestProgress(t, pool, recordingID); ok {
		t.Errorf("progress row still present after commit: %+v", row)
	}
}

// TestIngestWorker_ProgressRemainsAfterFailure は、転送は終わったがコミットに
// 至らなかったジョブが進捗行を残すことを確認する（issue #212）。
//
// 行が残ることは意図した挙動で、UI が「どこまで進んで止まっているか」を読む
// 唯一の材料になる（River の river_job を API 契約に露出させない代わり）。
// HEAD が転送量と食い違う長さを返すので、層 3 の照合で失敗する。
func TestIngestWorker_ProgressRemainsAfterFailure(t *testing.T) {
	tsData := makeTSData(100)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			// 転送量と食い違う長さ → size mismatch でジョブが失敗する。
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)+188))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/failing.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/failing.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	w := &IngestWorker{
		MirakcClient:     mirakc.NewClient(srv.URL, nil),
		MediaDir:         t.TempDir(),
		StallTimeout:     30 * time.Second,
		ProgressInterval: time.Millisecond,
		Pool:             pool,
	}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-failing")

	err := w.Work(context.Background(), &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-failing"},
	})
	if err == nil {
		t.Fatal("Work() succeeded, want size mismatch error")
	}

	row, ok := readIngestProgress(t, pool, recordingID)
	if !ok {
		t.Fatal("progress row was removed after a failed ingest; the UI would lose the only signal of how far it got")
	}
	if row.written != int64(len(tsData)) {
		t.Errorf("written_bytes after failure = %d, want %d", row.written, len(tsData))
	}
	// 分母は record_sync 由来。watcher が観測していなければ NULL のままにする
	// （HEAD の Content-Length で埋めない）。
	if row.expected != nil {
		t.Errorf("expected_bytes = %v, want nil (record_sync.content_length was never observed)", row.expected)
	}
}
