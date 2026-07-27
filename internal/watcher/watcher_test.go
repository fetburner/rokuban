package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// testIngestJobArgs は本パッケージのテスト専用 ingest ジョブ引数のスタブ。
//
// internal/watcher は internal/worker に依存できない（record_sweep ジョブが
// internal/worker から Watcher.Sweep を呼ぶため、逆方向に依存すると循環インポートに
// なる。watcher.go の IngestArgsFunc のコメント参照）。内部テストファイル
// （本ファイル、package watcher）も同じ制約を受けるため、internal/worker.IngestJobArgs
// を直接使う代わりに、同じ Kind（"ingest"）と UniqueOpts（ByArgs + 非最終状態限定）を
// 再現するスタブをここに置く。これにより、同一 record の repeated processRecord で
// ingest ジョブが重複しないという既存テストの前提が維持される。
type testIngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

func (testIngestJobArgs) Kind() string { return "ingest" }

func (testIngestJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			},
		},
	}
}

// testNewIngestArgs は Watcher.New に渡す IngestArgsFunc のテスト実装。
func testNewIngestArgs(site, recordID string) river.JobArgs {
	return testIngestJobArgs{Site: site, RecordID: recordID}
}

// testIngestWorker は testIngestJobArgs 用の no-op ワーカー。このパッケージの
// テストはジョブを実際に実行しない（river_job テーブルの行を SQL で確認するだけ）が、
// InsertTx は挿入時点で Kind が Workers バンドルに登録済みであることを要求するため、
// 何もしないワーカーだけ登録しておく。
type testIngestWorker struct {
	river.WorkerDefaults[testIngestJobArgs]
}

func (testIngestWorker) Work(context.Context, *river.Job[testIngestJobArgs]) error { return nil }

// newTestRiverClient はテスト用の River クライアントを作る。
//
// internal/worker.NewClient は使わない（同じ理由で internal/worker を import
// できないため）。
func newTestRiverClient(t *testing.T, pool *pgxpool.Pool) *river.Client[pgx5.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &testIngestWorker{})
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("creating test river client: %v", err)
	}
	return rc
}

func setupTest(t *testing.T) (*Watcher, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	mc := mirakc.NewClient("http://unused:40772", nil)
	w := New(DefaultSite, mc, pool, rc, testNewIngestArgs)
	return w, pool
}

func createTestReservation(t *testing.T, pool *pgxpool.Pool, programID int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
		VALUES ('default', $1, 'manual', 'Test Program', now(), 3600000)
		RETURNING id`, programID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	return id
}

func testRecord(recordID string, programID int64, reservationID int64, status string) mirakc.Record {
	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Test Program"

	r := mirakc.Record{
		ID: recordID,
		Program: mirakc.Program{
			ID:        programID,
			EventID:   100,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
		Service: mirakc.Service{
			Name:    "NHK総合",
			Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
		Tags: []string{mirakc.ReservationTag(reservationID)},
		Recording: mirakc.RecordInfo{
			Status:    status,
			StartTime: recStart,
		},
		Content: mirakc.ContentInfo{
			Path: "videos/test.m2ts",
			Type: "video/mp2t",
		},
	}

	if status == "finished" {
		endTime := mirakc.Milliseconds(time.Now())
		r.Recording.EndTime = &endTime
	}
	return r
}

func TestProcessRecord_CreateRecordingAndSync(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	resID := createTestReservation(t, pool, 327360102415397)
	record := testRecord("abc123def456", 327360102415397, resID, "finished")

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	// Verify record_sync
	var syncStatus string
	var syncRecordingID *int64
	err := pool.QueryRow(ctx,
		"SELECT status, recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID).Scan(&syncStatus, &syncRecordingID)
	if err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncStatus != "finished" {
		t.Errorf("record_sync.status = %q, want %q", syncStatus, "finished")
	}
	if syncRecordingID == nil {
		t.Fatal("record_sync.recording_id is nil")
	}

	// Verify recordings
	var recStatus string
	var recReservationID *int64
	err = pool.QueryRow(ctx,
		"SELECT status, reservation_id FROM recordings WHERE id = $1", *syncRecordingID,
	).Scan(&recStatus, &recReservationID)
	if err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "finished" {
		t.Errorf("recordings.status = %q, want %q", recStatus, "finished")
	}
	if recReservationID == nil || *recReservationID != resID {
		t.Errorf("recordings.reservation_id = %v, want %d", recReservationID, resID)
	}

	// Verify ingest job
	var jobCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount)
	if err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count = %d, want 1", jobCount)
	}
}

func TestProcessRecord_Idempotent(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	resID := createTestReservation(t, pool, 100001)
	record := testRecord("record-idem-001", 100001, resID, "finished")

	for i := 0; i < 3; i++ {
		if err := w.processRecord(ctx, record); err != nil {
			t.Fatalf("processRecord (iteration %d): %v", i, err)
		}
	}

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count = %d, want 1", recCount)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count = %d, want 1", jobCount)
	}
}

func TestProcessRecord_StatusProgression(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	resID := createTestReservation(t, pool, 100002)
	record := testRecord("record-prog-001", 100002, resID, "recording")

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (recording): %v", err)
	}

	// No ingest job while recording
	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("ingest job count during recording = %d, want 0", jobCount)
	}

	var recStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM recordings WHERE reservation_id = $1", resID).Scan(&recStatus); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "recording" {
		t.Errorf("recordings.status = %q, want %q", recStatus, "recording")
	}

	// Now finish
	record.Recording.Status = "finished"
	endTime := mirakc.Milliseconds(time.Now())
	record.Recording.EndTime = &endTime
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (finished): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT status FROM recordings WHERE reservation_id = $1", resID).Scan(&recStatus); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "finished" {
		t.Errorf("recordings.status = %q, want %q", recStatus, "finished")
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count after finished = %d, want 1", jobCount)
	}

	// Still only one recording
	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count = %d, want 1", recCount)
	}
}

func TestProcessRecord_StatusNoDowngrade(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	resID := createTestReservation(t, pool, 100003)
	record := testRecord("record-nodg-001", 100003, resID, "finished")

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (finished): %v", err)
	}

	// Out-of-order: recording event arrives after finished
	record.Recording.Status = "recording"
	record.Recording.EndTime = nil
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (recording): %v", err)
	}

	var recStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM recordings WHERE reservation_id = $1", resID).Scan(&recStatus); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "finished" {
		t.Errorf("recordings.status = %q, want %q (should not downgrade)", recStatus, "finished")
	}
}

func TestProcessRecord_UntaggedRecord(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	record := testRecord("record-notag-001", 100004, 0, "finished")
	record.Tags = nil

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	// record_sync created with nil recording_id
	var syncRecordingID *int64
	err := pool.QueryRow(ctx,
		"SELECT recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID).Scan(&syncRecordingID)
	if err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncRecordingID != nil {
		t.Errorf("expected recording_id nil for untagged record, got %d", *syncRecordingID)
	}

	// No recordings row
	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 0 {
		t.Errorf("recording count = %d, want 0 for untagged record", recCount)
	}

	// No ingest job
	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("ingest job count = %d, want 0 for untagged record", jobCount)
	}
}

// runConcurrentProcessRecord は同一 record を n 本の goroutine から同時に
// processRecord へ渡し、全 goroutine の完了を待ってエラーがないことを確認する。
// M2-16（processRecord の冪等化）の受け入れ基準である「並行実行しても
// recordings が 1 行しかできない」ことを検証するための土台。
func runConcurrentProcessRecord(t *testing.T, w *Watcher, record mirakc.Record, n int) {
	t.Helper()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.processRecord(ctx, record)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("processRecord (goroutine %d): %v", i, err)
		}
	}
}

// TestProcessRecord_ConcurrentIdempotent は M2-16 の受け入れ基準の核心を検証する。
// 同一 record を多数の goroutine から同時に processRecord して、record_sync の
// (site, record_id) 行ロック（AcquireRecordSync）による直列化によって recordings
// が 1 行しか作られないことを確認する。複数ラウンド（record_id を変えて繰り返す）
// 実行し、たまたま競合が起きなかっただけという flaky な成功を排除する。
func TestProcessRecord_ConcurrentIdempotent(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	const rounds = 30
	const goroutinesPerRound = 8

	for round := 0; round < rounds; round++ {
		programID := int64(400000 + round)
		recordID := fmt.Sprintf("record-concurrent-%03d", round)

		resID := createTestReservation(t, pool, programID)
		record := testRecord(recordID, programID, resID, "finished")
		// recordings には (site, network_id, service_id, event_id) の一意制約
		// （deleted_at IS NULL、00003_recordings_unique_active_event.sql）があるため、
		// ラウンドごとに event_id を変えて他ラウンドの録画と衝突しないようにする。
		record.Program.EventID = 500 + round

		runConcurrentProcessRecord(t, w, record, goroutinesPerRound)

		var recCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM recordings WHERE reservation_id = $1", resID,
		).Scan(&recCount); err != nil {
			t.Fatalf("round %d: querying recordings: %v", round, err)
		}
		if recCount != 1 {
			t.Fatalf("round %d: recording count = %d, want 1 (concurrent processRecord must be idempotent)", round, recCount)
		}

		var jobCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = 'ingest' AND args->>'record_id' = $1", recordID,
		).Scan(&jobCount); err != nil {
			t.Fatalf("round %d: querying river_job: %v", round, err)
		}
		if jobCount != 1 {
			t.Errorf("round %d: ingest job count = %d, want 1", round, jobCount)
		}
	}
}

// TestProcessRecord_ConcurrentUntaggedRecord は Rokuban 以外が mirakc に入れた
// tag のない record（record_sync.recording_id が NULL のまま正しい）を並行処理
// しても、recordings が作られたり record_sync が壊れたりしないことを検証する。
func TestProcessRecord_ConcurrentUntaggedRecord(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	record := testRecord("record-concurrent-notag-001", 400900, 0, "finished")
	record.Tags = nil

	runConcurrentProcessRecord(t, w, record, 20)

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 0 {
		t.Errorf("recording count = %d, want 0 for untagged record", recCount)
	}

	var syncRecordingID *int64
	if err := pool.QueryRow(ctx,
		"SELECT recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID).Scan(&syncRecordingID); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncRecordingID != nil {
		t.Errorf("expected recording_id nil for untagged record, got %d", *syncRecordingID)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("ingest job count = %d, want 0 for untagged record", jobCount)
	}
}

func TestHandleRecordBroken(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	resID := createTestReservation(t, pool, 100005)
	record := testRecord("record-broken-001", 100005, resID, "recording")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	if err := w.handleRecordBroken(ctx, mirakc.RecordBrokenData{
		RecordID: record.ID,
		Reason:   "io-error",
	}); err != nil {
		t.Fatalf("handleRecordBroken: %v", err)
	}

	var qeJSON json.RawMessage
	err := pool.QueryRow(ctx,
		"SELECT quality_events FROM recordings WHERE reservation_id = $1", resID,
	).Scan(&qeJSON)
	if err != nil {
		t.Fatalf("querying quality_events: %v", err)
	}

	var events []db.QualityEvent
	if err := json.Unmarshal(qeJSON, &events); err != nil {
		t.Fatalf("unmarshalling quality_events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 quality event, got %d", len(events))
	}
	if events[0].Event != "recording.record-broken" {
		t.Errorf("quality_events[0].event = %q, want %q", events[0].Event, "recording.record-broken")
	}
}

func TestHandleRecordingFailed_Idempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	programID := int64(327361024100)
	resID := createTestReservation(t, pool, programID)

	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Failed Program"

	schedule := mirakc.Schedule{
		State: "scheduled",
		Program: mirakc.Program{
			ID:        programID,
			EventID:   100,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
	}

	services := []mirakc.Service{
		{
			ServiceID: 1024,
			NetworkID: 32736,
			Name:      "NHK総合",
			Channel:   mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/schedules/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(schedule)
	})
	mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(services)
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, testNewIngestArgs)

	failedData := mirakc.RecordingFailedData{
		ProgramID: programID,
		Reason:    mirakc.FailedReason{Type: "tuner-unavailable"},
	}

	if err := w.handleRecordingFailed(ctx, failedData); err != nil {
		t.Fatalf("handleRecordingFailed (1st): %v", err)
	}

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings WHERE reservation_id = $1", resID).Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count after 1st call = %d, want 1", recCount)
	}

	// Call again with same program — should NOT create a duplicate
	if err := w.handleRecordingFailed(ctx, failedData); err != nil {
		t.Fatalf("handleRecordingFailed (2nd): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings WHERE reservation_id = $1", resID).Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count after 2nd call = %d, want 1 (idempotent)", recCount)
	}

	// Verify quality_events were merged (2 events appended)
	var qeJSON json.RawMessage
	if err := pool.QueryRow(ctx,
		"SELECT quality_events FROM recordings WHERE reservation_id = $1", resID,
	).Scan(&qeJSON); err != nil {
		t.Fatalf("querying quality_events: %v", err)
	}

	var events []db.QualityEvent
	if err := json.Unmarshal(qeJSON, &events); err != nil {
		t.Fatalf("unmarshalling quality_events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("quality_events count = %d, want 2 (merged from 2 calls)", len(events))
	}
}

func TestSweep_CatchesMissedRecords(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	res1ID := createTestReservation(t, pool, 200001)
	res2ID := createTestReservation(t, pool, 200002)

	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name1 := "Program 1"
	name2 := "Program 2"

	records := []mirakc.Record{
		{
			ID:        "reconcile-rec-001",
			Program:   mirakc.Program{ID: 200001, EventID: 1, ServiceID: 1, NetworkID: 1, StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name1},
			Service:   mirakc.Service{Name: "NHK", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}},
			Tags:      []string{mirakc.ReservationTag(res1ID)},
			Recording: mirakc.RecordInfo{Status: "finished", StartTime: recStart},
			Content:   mirakc.ContentInfo{Path: "test1.m2ts"},
		},
		{
			ID:        "reconcile-rec-002",
			Program:   mirakc.Program{ID: 200002, EventID: 2, ServiceID: 1, NetworkID: 1, StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name2},
			Service:   mirakc.Service{Name: "NHK", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}},
			Tags:      []string{mirakc.ReservationTag(res2ID)},
			Recording: mirakc.RecordInfo{Status: "finished", StartTime: recStart},
			Content:   mirakc.ContentInfo{Path: "test2.m2ts"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(records)
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, testNewIngestArgs)

	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var syncCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM record_sync").Scan(&syncCount); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncCount != 2 {
		t.Errorf("record_sync count = %d, want 2", syncCount)
	}

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 2 {
		t.Errorf("recordings count = %d, want 2", recCount)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 2 {
		t.Errorf("ingest job count = %d, want 2", jobCount)
	}

	// Run sweep again — verify idempotency
	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("sweep (2nd): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 2 {
		t.Errorf("recordings count after 2nd sweep = %d, want 2", recCount)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 2 {
		t.Errorf("ingest job count after 2nd sweep = %d, want 2", jobCount)
	}
}

// TestSweepAndHandleEvent_ConcurrentIdempotent は本タスク（M2-18）の核心を検証する。
// 3 段構え（docs/recording.md §3.3）のうち (a) SSE 由来の handleEvent と (c) 定期の
// Sweep（record_sweep ジョブから呼ばれる）が同一 record を同時に処理しても、
// M2-16 で processRecord に入れた record_sync の行ロックにより recordings が
// 重複しないことを確認する。
//
// (a) は record-saved イベントを模して handleEvent 経由で、(c) は Sweep（mirakc の
// ListRecords 経由）で、それぞれ独立した goroutine から同じ record を同時に叩く。
// 複数ラウンド実行して、たまたま競合が起きなかっただけの flaky な成功を排除する
// （TestProcessRecord_ConcurrentIdempotent と同じ考え方）。
func TestSweepAndHandleEvent_ConcurrentIdempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	const rounds = 20
	for round := 0; round < rounds; round++ {
		programID := int64(600000 + round)
		recordID := fmt.Sprintf("record-sweep-vs-event-%03d", round)

		resID := createTestReservation(t, pool, programID)
		record := testRecord(recordID, programID, resID, "finished")
		// recordings の (site, network_id, service_id, event_id) 一意制約
		// （deleted_at IS NULL）に他ラウンドと衝突しないよう event_id をずらす。
		record.Program.EventID = 900 + round

		mux := http.NewServeMux()
		// (c) Sweep が使う全量取得エンドポイント。
		mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode([]mirakc.Record{record})
		})
		// (a) handleEvent が record-saved を受けて GetRecord で個別取得するエンドポイント。
		mux.HandleFunc("/api/recording/records/"+recordID, func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(record)
		})
		// Sweep が呼ぶ ListServices 用のスタブ（未登録だと 404 がログに出るだけで
		// テスト結果には影響しないが、ノイズを消しておく）。
		mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode([]mirakc.Service{})
		})
		mockServer := httptest.NewServer(mux)

		w := New(DefaultSite, mirakc.NewClient(mockServer.URL, nil), pool, rc, testNewIngestArgs)

		savedData, err := json.Marshal(mirakc.RecordSavedData{
			RecordID:        recordID,
			RecordingStatus: "finished",
		})
		if err != nil {
			t.Fatalf("marshalling record-saved data: %v", err)
		}
		ev := mirakc.Event{Type: "recording.record-saved", Data: savedData}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// (a) SSE 由来: record-saved イベントを handleEvent 経由で処理する経路。
			w.handleEvent(ctx, ev)
		}()
		go func() {
			defer wg.Done()
			// (c) 定期突き合わせ: Sweep が ListRecords 経由で同じ record を処理する経路。
			if err := w.Sweep(ctx); err != nil {
				t.Errorf("round %d: Sweep: %v", round, err)
			}
		}()
		wg.Wait()
		mockServer.Close()

		var recCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM recordings WHERE reservation_id = $1", resID,
		).Scan(&recCount); err != nil {
			t.Fatalf("round %d: querying recordings: %v", round, err)
		}
		if recCount != 1 {
			t.Fatalf("round %d: recording count = %d, want 1 "+
				"((a) handleEvent と (c) Sweep の並行実行は冪等でなければならない)", round, recCount)
		}

		var jobCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = 'ingest' AND args->>'record_id' = $1", recordID,
		).Scan(&jobCount); err != nil {
			t.Fatalf("round %d: querying river_job: %v", round, err)
		}
		if jobCount != 1 {
			t.Errorf("round %d: ingest job count = %d, want 1", round, jobCount)
		}
	}
}

// TestRun_NoAutomaticSweep は watcher が SSE 購読と handleEvent だけの常駐になった
// こと（M2-18）を確認する。以前は Run 開始時に初回 reconcile を走らせ、以後も
// タイマーで定期 reconcile していたが、(c) を record_sweep ジョブへ切り出したことで
// Watcher 自身は `GET /api/recording/records` を一切呼ばなくなったはず。
func TestRun_NoAutomaticSweep(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	var recordsCalls, eventsCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&recordsCalls, 1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode([]mirakc.Record{})
	})
	mux.HandleFunc("/events", func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&eventsCalls, 1)
		rw.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := rw.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not Flusher")
		}
		flusher.Flush()
		<-r.Context().Done()
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, testNewIngestArgs)

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// SSE 接続が確立するまで少し待ってから、Run が自発的に全量突き合わせを
	// 呼んでいないことを確認する。
	deadline := time.After(500 * time.Millisecond)
	for atomic.LoadInt32(&eventsCalls) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE connection")
		case <-time.After(10 * time.Millisecond):
		}
	}

	runCancel()
	<-done

	if got := atomic.LoadInt32(&recordsCalls); got != 0 {
		t.Errorf("GET /api/recording/records call count = %d, want 0 "+
			"(Watcher.Run は SSE 購読だけの常駐になり、全量突き合わせは record_sweep ジョブの仕事のはず)", got)
	}
}
