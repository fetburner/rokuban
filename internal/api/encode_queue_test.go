package api

import (
	"context"
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

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

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

func TestGetEncodeQueueSummaryFromDomainState(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	queuedAndRunningID := seedRecording(t, pool, "待機と実行", base, "finished", 901)
	seedIngested(t, pool, queuedAndRunningID, 1000, nil)
	setEncodeProfiles(t, pool, queuedAndRunningID, []string{"h264", "h265"})
	seedEncodeAttempt(t, pool, queuedAndRunningID, "h264", "running", nil)

	observedID := seedRecording(t, pool, "完了済み", base.Add(time.Minute), "finished", 902)
	seedIngested(t, pool, observedID, 1000, nil)
	setEncodeProfiles(t, pool, observedID, []string{"h265"})
	seedEncodedAsset(t, pool, observedID, "h265", 1000)

	inactiveObservedID := seedRecording(t, pool, "削除中の encoded", base.Add(90*time.Second), "finished", 905)
	seedIngested(t, pool, inactiveObservedID, 1000, nil)
	setEncodeProfiles(t, pool, inactiveObservedID, []string{"h264"})
	if _, err := pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state)
		 VALUES ($1, 'encoded', 'h264', $2, 1000, 'deleting')`,
		inactiveObservedID, "test/inactive_observed.mp4"); err != nil {
		t.Fatalf("seeding deleting encoded media_asset: %v", err)
	}

	failedAttemptID := seedRecording(t, pool, "失敗済み試行", base.Add(2*time.Minute), "finished", 906)
	seedIngested(t, pool, failedAttemptID, 1000, nil)
	setEncodeProfiles(t, pool, failedAttemptID, []string{"h264"})
	seedEncodeAttempt(t, pool, failedAttemptID, "h264", "failed", nil)

	unknownProfileID := seedRecording(t, pool, "設定から消えた profile", base.Add(150*time.Second), "finished", 907)
	seedIngested(t, pool, unknownProfileID, 1000, nil)
	setEncodeProfiles(t, pool, unknownProfileID, []string{"removed"})

	inactiveOriginalID := seedRecording(t, pool, "active 原本なし", base.Add(3*time.Minute), "finished", 908)
	inactiveOriginalAssetID := seedIngested(t, pool, inactiveOriginalID, 1000, nil)
	setEncodeProfiles(t, pool, inactiveOriginalID, []string{"h264"})
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted' WHERE id = $1`, inactiveOriginalAssetID); err != nil {
		t.Fatalf("marking original inactive: %v", err)
	}

	deletedRecordingID := seedRecording(t, pool, "ごみ箱", base.Add(4*time.Minute), "finished", 909)
	seedIngested(t, pool, deletedRecordingID, 1000, nil)
	setEncodeProfiles(t, pool, deletedRecordingID, []string{"h264"})
	if _, err := pool.Exec(ctx,
		`UPDATE recordings SET deleted_at = now() WHERE id = $1`, deletedRecordingID); err != nil {
		t.Fatalf("soft-deleting recording: %v", err)
	}

	cutWithoutOwnershipID := seedRecording(t, pool, "未確認 cut", base.Add(2*time.Minute), "finished", 903)
	seedIngested(t, pool, cutWithoutOwnershipID, 1000, nil)
	setEncodeProfiles(t, pool, cutWithoutOwnershipID, []string{"cut"})

	cutOwnedID := seedRecording(t, pool, "確認済み cut", base.Add(3*time.Minute), "finished", 904)
	seedIngested(t, pool, cutOwnedID, 1000, nil)
	setEncodeProfiles(t, pool, cutOwnedID, []string{"cut"})
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, cutOwnedID); err != nil {
		t.Fatalf("seeding chapter ownership: %v", err)
	}

	srv := httptest.NewServer(NewRouter(RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "h265", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	var got struct {
		Queued  int64 `json:"queued"`
		Running int64 `json:"running"`
	}
	resp := getJSON(t, srv.URL+"/api/encode-queue", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/encode-queue status = %d, want 200", resp.StatusCode)
	}
	if got.Queued != 4 || got.Running != 1 {
		t.Errorf("encode queue summary = queued %d, running %d; want queued 4, running 1", got.Queued, got.Running)
	}

	missing, err := sqlcgen.New(pool).ListMissingEncodeProfiles(ctx, sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles:    []string{"h264", "h265", "cut"},
		CutProfiles:      []string{"cut"},
		AfterRecordingID: 0,
		RowLimit:         100,
	})
	if err != nil {
		t.Fatalf("listing missing encode profiles: %v", err)
	}
	if got.Queued != int64(len(missing)-1) {
		t.Errorf("queued = %d, ListMissingEncodeProfiles has %d rows and one running profile; want %d",
			got.Queued, len(missing), len(missing)-1)
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
