package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

func TestGetEncodeQueueSummaryCountsJobsNotRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	client, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	queuedRecordingID := seedRecording(t, pool, "待機中", base, "finished", 901)
	runningRecordingID := seedRecording(t, pool, "実行中", base.Add(time.Minute), "finished", 902)
	completedRecordingID := seedRecording(t, pool, "完了済み", base.Add(2*time.Minute), "finished", 903)

	insert := func(recordingID int64, profile, state string) {
		t.Helper()
		result, err := client.Insert(ctx, worker.EncodeJobArgs{RecordingID: recordingID, Profile: profile}, nil)
		if err != nil {
			t.Fatalf("inserting %s encode job: %v", state, err)
		}
		if state == "available" {
			return
		}
		var update string
		if state == "completed" {
			update = `UPDATE river_job SET state = $1, finalized_at = now() WHERE id = $2`
		} else {
			update = `UPDATE river_job SET state = $1 WHERE id = $2`
		}
		if _, err := pool.Exec(ctx, update, state, result.Job.ID); err != nil {
			t.Fatalf("setting encode job %d state to %s: %v", result.Job.ID, state, err)
		}
	}
	insert(queuedRecordingID, "h264", "available")
	insert(queuedRecordingID, "h265", "retryable")
	insert(runningRecordingID, "h264", "running")
	insert(completedRecordingID, "h264", "completed")

	srv := newAPIServer(t, pool)
	var got struct {
		Queued  int64 `json:"queued"`
		Running int64 `json:"running"`
	}
	resp := getJSON(t, srv.URL+"/api/encode-queue", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/encode-queue status = %d, want 200", resp.StatusCode)
	}
	if got.Queued != 2 || got.Running != 1 {
		t.Errorf("encode queue summary = queued %d, running %d; want queued 2, running 1", got.Queued, got.Running)
	}
}

func TestListRecordingsEncodeStateFilterUsesRiverJobs(t *testing.T) {
	pool := testutil.SetupDB(t)
	client, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	queuedID := seedRecording(t, pool, "待機中", base, "finished", 911)
	runningID := seedRecording(t, pool, "実行中", base.Add(time.Minute), "finished", 912)
	seedRecording(t, pool, "ジョブなし", base.Add(2*time.Minute), "finished", 913)

	queued, err := client.Insert(ctx, worker.EncodeJobArgs{RecordingID: queuedID, Profile: "h264"}, nil)
	if err != nil {
		t.Fatalf("inserting queued encode job: %v", err)
	}
	running, err := client.Insert(ctx, worker.EncodeJobArgs{RecordingID: runningID, Profile: "h264"}, nil)
	if err != nil {
		t.Fatalf("inserting running encode job: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'running' WHERE id = $1`, running.Job.ID); err != nil {
		t.Fatalf("setting encode job running: %v", err)
	}
	_ = queued

	srv := newAPIServer(t, pool)
	queuedTitles := getRecordingsTitles(t, srv.URL, url.Values{"encodeState": {"queued"}})
	if len(queuedTitles) != 1 || queuedTitles[0] != "待機中" {
		t.Errorf("encodeState=queued got %v, want [待機中]", queuedTitles)
	}
	runningTitles := getRecordingsTitles(t, srv.URL, url.Values{"encodeState": {"running"}})
	if len(runningTitles) != 1 || runningTitles[0] != "実行中" {
		t.Errorf("encodeState=running got %v, want [実行中]", runningTitles)
	}
}

func TestListRecordingsRejectsUnknownEncodeState(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	resp := getJSON(t, srv.URL+"/api/recordings?encodeState=finished", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("encodeState=finished status = %d, want 400", resp.StatusCode)
	}
}
