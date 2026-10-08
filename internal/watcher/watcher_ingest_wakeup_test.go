package watcher

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
)

type snoozingIngestTestWorker struct {
	river.WorkerDefaults[jobs.IngestJobArgs]
	entered chan struct{}
	once    sync.Once
}

func (w *snoozingIngestTestWorker) Work(context.Context, *river.Job[jobs.IngestJobArgs]) error {
	w.once.Do(func() { close(w.entered) })
	return river.JobSnooze(time.Hour)
}

func insertSnoozedIngestJob(t *testing.T, pool *pgxpool.Pool, args jobs.IngestJobArgs) (int64, time.Time) {
	t.Helper()

	workers := river.NewWorkers()
	testWorker := &snoozingIngestTestWorker{entered: make(chan struct{})}
	river.AddWorker(workers, testWorker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.PhysicalQueueName(jobs.IngestQueue, args.Site): {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("creating snoozing River client: %v", err)
	}

	clientCtx, cancel := context.WithCancel(context.Background())
	if err := client.Start(clientCtx); err != nil {
		cancel()
		t.Fatalf("starting snoozing River client: %v", err)
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-client.Stopped()
		})
	}
	t.Cleanup(stop)

	inserted, err := client.Insert(clientCtx, args, nil)
	if err != nil {
		t.Fatalf("inserting ingest job: %v", err)
	}
	if inserted.Job == nil {
		t.Fatal("inserted ingest job is nil")
	}

	select {
	case <-testWorker.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ingest worker to snooze")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		var scheduledAt time.Time
		err := pool.QueryRow(context.Background(),
			"SELECT state, scheduled_at FROM river_job WHERE id = $1", inserted.Job.ID,
		).Scan(&state, &scheduledAt)
		if err != nil {
			t.Fatalf("querying snoozed ingest job: %v", err)
		}
		if state == string(rivertype.JobStateScheduled) {
			stop()
			if !scheduledAt.After(time.Now()) {
				t.Fatalf("scheduled_at = %s, want future time for a one-hour snooze", scheduledAt)
			}
			return inserted.Job.ID, scheduledAt
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest job state = %q, want scheduled after JobSnooze", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readRiverJobState(t *testing.T, pool *pgxpool.Pool, jobID int64) (string, time.Time) {
	t.Helper()
	var state string
	var scheduledAt time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT state, scheduled_at FROM river_job WHERE id = $1", jobID,
	).Scan(&state, &scheduledAt); err != nil {
		t.Fatalf("querying River job %d: %v", jobID, err)
	}
	return state, scheduledAt
}

func TestProcessRecord_WakesSnoozedIngestWhenLateSHA256Arrives(t *testing.T) {
	w, pool := setupTest(t)
	const programID int64 = 780001
	createTestReservation(t, pool, programID)

	jobID, _ := insertSnoozedIngestJob(t, pool, jobs.IngestJobArgs{
		Site: DefaultSite, RecordID: "record-wake-sha256-001",
	})
	record := testRecord("record-wake-sha256-001", programID, "finished")
	hash := strings.Repeat("a", 64)
	record.Content.Sha256 = &hash

	originalLogger := slog.Default()
	var logOutput strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	if err := w.processRecord(context.Background(), record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	state, scheduledAt := readRiverJobState(t, pool, jobID)
	if state != string(rivertype.JobStateAvailable) {
		t.Fatalf("ingest job state = %q, want available after late SHA-256", state)
	}
	if scheduledAt.After(time.Now()) {
		t.Errorf("scheduled_at = %s, want now or earlier after retry", scheduledAt)
	}
	if got := logOutput.String(); !strings.Contains(got, "record_id="+record.ID) || !strings.Contains(got, "job_id="+strconv.FormatInt(jobID, 10)) {
		t.Errorf("wake log = %q, want record_id and job_id", got)
	}
}

func TestProcessRecord_LeavesSnoozedIngestWhenSHA256IsNull(t *testing.T) {
	w, pool := setupTest(t)
	const programID int64 = 780002
	createTestReservation(t, pool, programID)

	jobID, originalScheduledAt := insertSnoozedIngestJob(t, pool, jobs.IngestJobArgs{
		Site: DefaultSite, RecordID: "record-wake-sha256-null-001",
	})
	record := testRecord("record-wake-sha256-null-001", programID, "finished")
	record.Content.Sha256 = nil

	if err := w.processRecord(context.Background(), record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	state, scheduledAt := readRiverJobState(t, pool, jobID)
	if state != string(rivertype.JobStateScheduled) {
		t.Fatalf("ingest job state = %q, want scheduled while SHA-256 is null", state)
	}
	if !scheduledAt.Equal(originalScheduledAt) {
		t.Errorf("scheduled_at = %s, want unchanged snooze deadline %s", scheduledAt, originalScheduledAt)
	}
}

type fakeWatcherRiverClient struct {
	insertResult *rivertype.JobInsertResult
	retryIDs     []int64
}

func (f *fakeWatcherRiverClient) InsertTx(context.Context, pgx5.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return f.insertResult, nil
}

func (f *fakeWatcherRiverClient) JobRetryTx(_ context.Context, _ pgx5.Tx, id int64) (*rivertype.JobRow, error) {
	f.retryIDs = append(f.retryIDs, id)
	return &rivertype.JobRow{ID: id, State: rivertype.JobStateAvailable}, nil
}

// completed/discarded are normally outside the ingest uniqueness states; these synthetic results
// exercise the watcher guard if River ever returns a duplicate row in one of those states.
func TestProcessRecord_RetriesOnlyScheduledIngestWithFinishedHash(t *testing.T) {
	for _, tt := range []struct {
		name          string
		state         rivertype.JobState
		duplicate     bool
		hasHash       bool
		recordStatus  string
		wantRetryCall bool
	}{
		{name: "scheduled with hash", state: rivertype.JobStateScheduled, duplicate: true, hasHash: true, recordStatus: "finished", wantRetryCall: true},
		{name: "scheduled without hash", state: rivertype.JobStateScheduled, duplicate: true, recordStatus: "finished"},
		{name: "running", state: rivertype.JobStateRunning, duplicate: true, hasHash: true, recordStatus: "finished"},
		{name: "completed", state: rivertype.JobStateCompleted, duplicate: true, hasHash: true, recordStatus: "finished"},
		{name: "discarded", state: rivertype.JobStateDiscarded, duplicate: true, hasHash: true, recordStatus: "finished"},
		{name: "new insert", state: rivertype.JobStateScheduled, hasHash: true, recordStatus: "finished"},
		{name: "recording status", state: rivertype.JobStateScheduled, duplicate: true, hasHash: true, recordStatus: "recording"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, pool := setupTest(t)
			const programID int64 = 780003
			createTestReservation(t, pool, programID)
			fake := &fakeWatcherRiverClient{insertResult: &rivertype.JobInsertResult{
				Job:                      &rivertype.JobRow{ID: 987654, State: tt.state},
				UniqueSkippedAsDuplicate: tt.duplicate,
			}}
			w.river = fake

			record := testRecord("record-wake-guard-001", programID, tt.recordStatus)
			if tt.hasHash {
				hash := strings.Repeat("b", 64)
				record.Content.Sha256 = &hash
			}
			if err := w.processRecord(context.Background(), record); err != nil {
				t.Fatalf("processRecord: %v", err)
			}

			if tt.wantRetryCall {
				if len(fake.retryIDs) != 1 || fake.retryIDs[0] != 987654 {
					t.Fatalf("JobRetryTx IDs = %v, want [987654]", fake.retryIDs)
				}
			} else if len(fake.retryIDs) != 0 {
				t.Errorf("JobRetryTx IDs = %v, want no retry", fake.retryIDs)
			}
		})
	}
}
