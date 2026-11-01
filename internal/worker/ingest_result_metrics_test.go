package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/jobqfake"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

// このファイルは IngestWorker.Work の結果分類を固定する。実行コンテキストの
// キャンセルは数えず、録画取り消しは canceled、本当の失敗は failure として数える。
// 判定は Work の defer で context.Cause(ctx) と返す err から行う。

// ingestJobResultValues は IngestJobs の result ラベル値の全体。増やすときは
// docs/operations/monitoring.md と internal/metrics/metrics.go の説明も揃える。
var ingestJobResultValues = []string{"success", "failure", "canceled"}

// ingestJobResults は 3 値の現在値を読む。WithLabelValues は未作成の組み合わせを
// 作るので、before / after のどちらでも同じ形で読める。
func ingestJobResults() map[string]float64 {
	results := make(map[string]float64, len(ingestJobResultValues))
	for _, result := range ingestJobResultValues {
		results[result] = promtestutil.ToFloat64(metrics.IngestJobs.WithLabelValues(result))
	}
	return results
}

// assertIngestResultDeltas は 3 値すべての増分を確認する。want に無い値は 0 と
// みなす。「1 つだけ見る」テストにしないのは、数えないことが期待される経路で
// 別の値に落ちる変異を見逃すため。
func assertIngestResultDeltas(t *testing.T, before map[string]float64, want map[string]float64, when string) {
	t.Helper()
	for _, result := range ingestJobResultValues {
		got := promtestutil.ToFloat64(metrics.IngestJobs.WithLabelValues(result))
		if expected := before[result] + want[result]; got != expected {
			t.Errorf("%s: IngestJobs{result=%q} = %v, want %v", when, result, got, expected)
		}
	}
}

// ingestDurationSamples は IngestDuration の観測数（Prometheus の _count）を返す。
// ToFloat64 はヒストグラムの合計を返すので使えない。
func ingestDurationSamples(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.IngestDuration.Write(&m); err != nil {
		t.Fatalf("reading IngestDuration: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestIngestWorker_DeadlineIsCountedAsFailure(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient("http://127.0.0.1:1", nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	beforeResults := ingestJobResults()
	durationBefore := ingestDurationSamples(t)
	err := runWorkerViaJobQFake(ctx, w,

		IngestJobArgs{Site: "default", RecordID: "rec-timeout-metrics"},
	)
	if err == nil {
		t.Fatal("Work() with an expired deadline returned nil")
	}
	assertIngestResultDeltas(t, beforeResults, map[string]float64{"failure": 1}, "deadline timeout")
	if got, want := ingestDurationSamples(t), durationBefore+1; got != want {
		t.Errorf("IngestDuration samples after deadline = %d, want %d", got, want)
	}
}

func TestIngestWorker_SHA256WaitSnoozesJobWithoutMetrics(t *testing.T) {
	tsData := makeTSData(20)
	setIngestSHA256Rate(t, 47)
	const recordID = "rec-sha256-river-complete"
	fake, _ := newLateHashIngestFake(t, recordID, tsData, "test/sha-wait.m2ts")
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)
	mediaDir := t.TempDir()
	delay := time.Duration(0)
	workResult := make(chan error, 1)
	registry := jobq.NewRegistry()
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		MediaDir:      mediaDir,
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	jobq.Register(registry, snoozeResultCapturingIngestWorker{worker: w, result: workResult})
	queue := jobqfake.New(registry, jobqfake.Config{SnoozeDelay: &delay})
	w.Enqueuer = queue
	if err := queue.Start(context.Background()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := queue.Stop(stopCtx); err != nil {
			t.Errorf("queue.Stop: %v", err)
		}
	})

	beforeResults := ingestJobResults()
	durationBefore := ingestDurationSamples(t)
	_, err := queue.Enqueue(context.Background(), jobs.IngestJobArgs{Site: "default", RecordID: recordID})
	if err != nil {
		t.Fatalf("inserting ingest job: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(queue.Deliveries()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	deliveries := queue.Deliveries()
	if len(deliveries) != 1 {
		t.Fatalf("ingest job deliveries before completion = %d, want exactly 1", len(deliveries))
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelWait()
	if err := queue.WaitIdle(waitCtx); err != nil {
		t.Fatalf("queue.WaitIdle: %v", err)
	}
	result := <-workResult
	if got, ok := jobq.SnoozeDelay(result); !ok || got != 30*time.Second {
		t.Fatalf("IngestWorker.Work snooze = %v, %v; want 30s while content.sha256 is null", got, ok)
	}
	assertIngestResultDeltas(t, beforeResults, nil, "SHA-256 pending snooze")
	if got := ingestDurationSamples(t); got != durationBefore {
		t.Errorf("IngestDuration samples after pending snooze = %d, want %d", got, durationBefore)
	}

	var assetCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID).Scan(&assetCount); err != nil {
		t.Fatalf("counting media_assets while waiting: %v", err)
	}
	if assetCount != 0 {
		t.Errorf("media_assets rows while waiting = %d, want 0", assetCount)
	}
	if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 0 {
		t.Errorf("DeleteRecord attempts while waiting = %d, want 0", got)
	}
	var progressRows int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM recording_ingest_progress WHERE recording_id = $1", recordingID).Scan(&progressRows); err != nil {
		t.Fatalf("counting progress rows while waiting: %v", err)
	}
	if progressRows != 1 {
		t.Errorf("recording_ingest_progress rows while waiting = %d, want 1 (kept until commit)", progressRows)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, "sites", "default", "test"), "default", recordID)
	if info, err := os.Stat(tempPath); err != nil || info.Size() != int64(len(tsData)) {
		t.Errorf("ingest temp while waiting: stat=(%v, %v), want size %d", info, err, len(tsData))
	}
	var handedOff bool
	var offset int64
	var hashState []byte
	if err := pool.QueryRow(context.Background(), `
SELECT handed_off, offset_bytes, hash_state
FROM recording_ingest_checkpoints WHERE recording_id = $1`, recordingID).Scan(&handedOff, &offset, &hashState); err != nil {
		t.Fatalf("reading DB SHA-256 checkpoint while waiting: %v", err)
	}
	if !handedOff || offset != int64(len(tsData)) || len(hashState) == 0 {
		t.Errorf("DB SHA-256 checkpoint while waiting = (handed_off=%t, offset=%d, state bytes=%d), want clean checkpoint at %d", handedOff, offset, len(hashState), len(tsData))
	}
	if _, err := os.Stat(tempPath + ".checkpoint"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy sidecar checkpoint stat error = %v, want no sidecar file", err)
	}

}

type snoozeResultCapturingIngestWorker struct {
	worker *IngestWorker
	result chan<- error
}

func (worker snoozeResultCapturingIngestWorker) Work(ctx context.Context, args jobs.IngestJobArgs) error {
	err := worker.worker.Work(ctx, args)
	worker.result <- err
	return nil
}

// TestIngestWorker_ContextCancellationIsNotCounted fixes the metric behavior for a
// canceled context without depending on a queue-specific cancellation type.
func TestIngestWorker_ContextCancellationIsNotCounted(t *testing.T) {
	t.Run("Work returns an error", func(t *testing.T) {
		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		fake := mirakctest.New(t)

		w := &IngestWorker{
			Publisher:     pgstoretest.Media(pool),
			MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			RecordTarget:  pgstoretest.Recordings(pool),
			Precheck:      pgstoretest.Media(pool),
		}
		recordingID := dbtest.Recording(t, pool)
		insertTestRecordSync(t, pool, recordingID, "rec-remote-cancel-err")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		before := ingestJobResults()
		err := runWorkerViaJobQFake(ctx, w,

			IngestJobArgs{Site: "default", RecordID: "rec-remote-cancel-err"},
		)
		if err == nil {
			t.Fatal("Work() = nil, want an error（ctx を取り消した状態では最初の DB 取得で失敗する）")
		}
		assertIngestResultDeltas(t, before, nil, "context cancellation with error")
	})

	t.Run("Work returns nil", func(t *testing.T) {
		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		const recordID = "rec-remote-cancel-nil"
		fake := mirakctest.New(t)
		fake.SetRecords([]mirakc.Record{{ID: recordID}})
		client := &http.Client{Transport: mirakcRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			// 原本が既にあるので Work は DeleteRecord を呼ぶ。要求を出す直前に
			// context を取り消し、HTTP 呼び出しの失敗を再現する。
			cancel()
			return http.DefaultTransport.RoundTrip(req)
		})}

		w := &IngestWorker{
			Publisher:     pgstoretest.Media(pool),
			MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), client)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			RecordTarget:  pgstoretest.Recordings(pool),
			Precheck:      pgstoretest.Media(pool),
		}
		recordingID := dbtest.Recording(t, pool)
		insertTestRecordSync(t, pool, recordingID, recordID)
		dbtest.MediaAsset(t, pool, recordingID)

		before := ingestJobResults()
		err := runWorkerViaJobQFake(ctx, w,

			IngestJobArgs{Site: "default", RecordID: recordID},
		)
		if err != nil {
			t.Fatalf("Work() = %v, want nil", err)
		}
		assertIngestResultDeltas(t, before, map[string]float64{"success": 1}, "context cancellation with nil result")
	})

	t.Run("Work returns the context cause", func(t *testing.T) {
		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/") {
				cancel(errors.New("adapter stop"))
				<-r.Context().Done()
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		w := &IngestWorker{
			Publisher:     pgstoretest.Media(pool),
			MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			RecordTarget:  pgstoretest.Recordings(pool),
			Precheck:      pgstoretest.Media(pool),
		}
		recordingID := dbtest.Recording(t, pool)
		insertTestRecordSync(t, pool, recordingID, "rec-remote-cancel-cause")

		before := ingestJobResults()
		durationBefore := ingestDurationSamples(t)
		// net/http は取り消し時に context.Cause(ctx) を返し、それは context.Canceled を
		// 包まない。だから ingest の分類は errors.Is(err, cause) の項も見る必要がある。
		_ = runWorkerViaJobQFake(ctx, w,
			IngestJobArgs{Site: "default", RecordID: "rec-remote-cancel-cause"},
		)
		assertIngestResultDeltas(t, before, nil, "context cancellation with cause")
		if got := ingestDurationSamples(t); got != durationBefore {
			t.Errorf("IngestDuration samples = %d, want %d (unchanged)", got, durationBefore)
		}
	})
}

func TestIngestWorker_FollowupEnqueueFailuresAreLoggedAndIgnored(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	tsData := makeTSData(2)
	recordingID := dbtest.Recording(t, pool)
	const recordID = "rec-followup-enqueue-failure"
	insertTestRecordSync(t, pool, recordingID, recordID)
	fake := newFullTransferFake(t, recordID, tsData, "test/followup-enqueue-failure.m2ts")

	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	worker := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		Enqueuer:      &jobq.Deferred{},
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      t.TempDir(),
		Precheck:      pgstoretest.Media(pool),
	}
	worker.CMDetectionStore = pgstoretest.Analysis(pool)
	if err := runWorkerViaJobQFake(context.Background(), worker,
		IngestJobArgs{Site: "default", RecordID: recordID}); err != nil {
		t.Fatalf("Work() = %v, want best-effort enqueue failures to be ignored", err)
	}
	for _, want := range []string{
		"encode_reconcile: failed to enqueue targeted pass",
		"ingest: failed to enqueue thumbnail reconcile",
		"ingest: failed to enqueue TS scan",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs do not contain %q: %s", want, logs.String())
		}
	}
	if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 1 {
		t.Errorf("DeleteRecord calls = %d, want 1 after best-effort enqueue failures", got)
	}
}
