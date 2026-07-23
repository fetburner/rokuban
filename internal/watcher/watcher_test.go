package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

func setupTest(t *testing.T) (*Watcher, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := worker.NewWorkers()
	rc, err := worker.NewClient(pool, workers)
	if err != nil {
		t.Fatalf("creating river client: %v", err)
	}

	mc := mirakc.NewClient("http://unused:40772", nil)
	w := New(DefaultSite, mc, pool, rc, nil)
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

func TestReconcile_CatchesMissedRecords(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning: %v", err)
	}

	workers := worker.NewWorkers()
	rc, err := worker.NewClient(pool, workers)
	if err != nil {
		t.Fatalf("river client: %v", err)
	}

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
	w := New(DefaultSite, mc, pool, rc, nil)

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
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

	// Run reconcile again — verify idempotency
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile (2nd): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 2 {
		t.Errorf("recordings count after 2nd reconcile = %d, want 2", recCount)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 2 {
		t.Errorf("ingest job count after 2nd reconcile = %d, want 2", jobCount)
	}
}
