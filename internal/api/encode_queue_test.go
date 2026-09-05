package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

type encodeQueueTestWorker struct {
	river.WorkerDefaults[jobs.EncodeJobArgs]
	err       error
	nextRetry time.Time
}

func (w *encodeQueueTestWorker) Work(_ context.Context, _ *river.Job[jobs.EncodeJobArgs]) error {
	return w.err
}

func (w *encodeQueueTestWorker) NextRetry(_ *river.Job[jobs.EncodeJobArgs]) time.Time {
	return w.nextRetry
}

type blockingEncodeQueueTestWorker struct {
	river.WorkerDefaults[jobs.EncodeJobArgs]
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
}

func (w *blockingEncodeQueueTestWorker) Work(ctx context.Context, _ *river.Job[jobs.EncodeJobArgs]) error {
	w.startedOnce.Do(func() { close(w.started) })
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Timeout はジョブ既定の JobTimeoutDefault（1 分）を無効化する。テストは
// MaxWorkers: 1 の枠をこの worker がブロックしたまま別ジョブを available に
// 残す構成なので、既定タイムアウトが効くと job ctx がキャンセルされて Work が
// ctx.Err() を返し、ジョブが retryable に落ちて枠が空く。その状態で producer が
// 残りの available ジョブを即座に fetch すると、started の 2 回目の close で
// panic する。
func (w *blockingEncodeQueueTestWorker) Timeout(*river.Job[jobs.EncodeJobArgs]) time.Duration {
	return -1
}

func workEncodeJob(t *testing.T, pool *pgxpool.Pool, job *rivertype.JobRow, workErr error) *rivertest.WorkResult {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning transaction for encode job: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // harmless after a successful commit

	testJobWorker := &encodeQueueTestWorker{err: workErr}
	if workErr != nil {
		// River turns a retry shorter than the scheduler interval into an
		// immediately available job. Keep this retry in the retryable state so
		// the test exercises the API's retryable-state mapping.
		testJobWorker.nextRetry = time.Now().Add(time.Minute)
	}
	testWorker := rivertest.NewWorker[jobs.EncodeJobArgs, pgx.Tx](
		t, riverpgxv5.New(pool), &river.Config{}, testJobWorker,
	)
	result, gotErr := testWorker.WorkJob(ctx, t, tx, job)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing encode job transition: %v", err)
	}
	if workErr == nil && gotErr != nil {
		t.Fatalf("working encode job: %v", gotErr)
	}
	if workErr != nil && gotErr == nil {
		t.Fatalf("working encode job returned nil error, want %v", workErr)
	}
	if workErr != nil && !errors.Is(gotErr, workErr) {
		t.Fatalf("working encode job returned error %v, want %v", gotErr, workErr)
	}
	if result == nil {
		t.Fatalf("working encode job returned nil result (gotErr=%v)", gotErr)
	}
	return result
}

func startBlockingEncodeClient(t *testing.T, pool *pgxpool.Pool) (*river.Client[pgx.Tx], context.CancelFunc, *blockingEncodeQueueTestWorker) {
	t.Helper()
	workers := river.NewWorkers()
	testWorker := &blockingEncodeQueueTestWorker{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	river.AddWorker(workers, testWorker)

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{"encode": {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("creating blocking encode river client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := client.Start(ctx); err != nil {
		cancel()
		t.Fatalf("starting blocking encode river client: %v", err)
	}
	return client, cancel, testWorker
}

func waitForBlockingEncodeWorker(t *testing.T, testWorker *blockingEncodeQueueTestWorker) {
	t.Helper()
	select {
	case <-testWorker.started:
	case <-time.After(20 * time.Second):
		t.Fatal("blocking encode worker did not start")
	}
}

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
		result, err := client.Insert(ctx, jobs.EncodeJobArgs{RecordingID: recordingID, Profile: profile}, nil)
		if err != nil {
			t.Fatalf("inserting %s encode job: %v", state, err)
		}
		if state == "available" {
			return
		}
		switch state {
		case "retryable":
			worked := workEncodeJob(t, pool, result.Job, errors.New("encode queue test: retryable"))
			if worked.EventKind != river.EventKindJobFailed || worked.Job.State != rivertype.JobStateRetryable {
				t.Fatalf("retryable encode job result = event %q, state %q; want failed, retryable",
					worked.EventKind, worked.Job.State)
			}
		case "completed":
			worked := workEncodeJob(t, pool, result.Job, nil)
			if worked.EventKind != river.EventKindJobCompleted || worked.Job.State != rivertype.JobStateCompleted {
				t.Fatalf("completed encode job result = event %q, state %q; want completed, completed",
					worked.EventKind, worked.Job.State)
			}
		default:
			t.Fatalf("unsupported encode job state %q", state)
		}
	}
	insert(queuedRecordingID, "h265", "retryable")
	if _, err := client.Insert(ctx, jobs.EncodeJobArgs{RecordingID: runningRecordingID, Profile: "h264"}, nil); err != nil {
		t.Fatalf("inserting running encode job: %v", err)
	}
	runningClient, runningCancel, runningWorker := startBlockingEncodeClient(t, pool)
	defer func() {
		runningCancel()
		close(runningWorker.release)
		<-runningClient.Stopped()
	}()
	waitForBlockingEncodeWorker(t, runningWorker)
	insert(queuedRecordingID, "h264", "available")
	insert(completedRecordingID, "h264", "completed")

	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, RiverClient: client}))
	t.Cleanup(srv.Close)
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

	runningClient, runningCancel, runningWorker := startBlockingEncodeClient(t, pool)
	defer func() {
		runningCancel()
		close(runningWorker.release)
		<-runningClient.Stopped()
	}()
	if _, err := runningClient.Insert(ctx, jobs.EncodeJobArgs{RecordingID: runningID, Profile: "h264"}, nil); err != nil {
		t.Fatalf("inserting running encode job: %v", err)
	}
	waitForBlockingEncodeWorker(t, runningWorker)
	if _, err := client.Insert(ctx, jobs.EncodeJobArgs{RecordingID: queuedID, Profile: "h264"}, nil); err != nil {
		t.Fatalf("inserting queued encode job: %v", err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, RiverClient: client}))
	t.Cleanup(srv.Close)
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
