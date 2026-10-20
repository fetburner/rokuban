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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/jobqfake"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store/pgstore"
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

// insertTestOriginalMediaAsset は録画に原本 media_asset を 1 行足す。Work の
// 冪等性チェック（hasOriginalMediaAsset）を通して handleAlreadyCommittedIngest
// へ入れるようにするためで、他の意味は無い。
func insertTestOriginalMediaAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "sites/default/test/already-committed.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("inserting original media_asset: %v", err)
	}
}

func TestIngestWorker_DeadlineIsCountedAsFailure(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mirakc.NewClient("http://127.0.0.1:1", nil)),
		Pool:          pool,
		RecordTarget:  pgstore.NewRecordings(pool),
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
	var deleteAttempts atomic.Int32
	srv, _ := newLateHashIngestServer(t, tsData, "test/sha-wait.m2ts", nil, func() {
		deleteAttempts.Add(1)
	})
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := insertTestRecording(t, pool)
	recordID := "rec-sha256-river-complete"
	insertTestRecordSync(t, pool, recordingID, recordID)
	mediaDir := t.TempDir()
	delay := time.Duration(0)
	workResult := make(chan error, 1)
	registry := jobq.NewRegistry()
	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      mediaDir,
		StallTimeout:  time.Second,
		Pool:          pool,
		RecordTarget:  pgstore.NewRecordings(pool),
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
	if got := deleteAttempts.Load(); got != 0 {
		t.Errorf("DeleteRecord attempts while waiting = %d, want 0", got)
	}
	var progressRows int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM recording_ingest_progress WHERE recording_id = $1", recordingID).Scan(&progressRows); err != nil {
		t.Fatalf("counting progress rows while waiting: %v", err)
	}
	if progressRows != 0 {
		t.Errorf("recording_ingest_progress rows while waiting = %d, want 0", progressRows)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, "sites", "default", "test"), "default", recordID)
	if info, err := os.Stat(tempPath); err != nil || info.Size() != int64(len(tsData)) {
		t.Errorf("ingest temp while waiting: stat=(%v, %v), want size %d", info, err, len(tsData))
	}
	if _, err := os.Stat(ingestCheckpointPath(tempPath)); err != nil {
		t.Errorf("SHA-256 checkpoint while waiting: %v, want present", err)
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		w := &IngestWorker{
			MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			Pool:          pool,
			RecordTarget:  pgstore.NewRecordings(pool),
		}
		recordingID := insertTestRecording(t, pool)
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

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 原本が既にあるので Work は転送せず handleAlreadyCommittedIngest へ入る。
			// その最初の HTTP 要求（DeleteRecord）で取り消すと、削除の失敗はログだけ
			// なので Work は nil を返す。
			cancel()
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		w := &IngestWorker{
			MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			Pool:          pool,
			RecordTarget:  pgstore.NewRecordings(pool),
		}
		recordingID := insertTestRecording(t, pool)
		insertTestRecordSync(t, pool, recordingID, "rec-remote-cancel-nil")
		insertTestOriginalMediaAsset(t, pool, recordingID)

		before := ingestJobResults()
		err := runWorkerViaJobQFake(ctx, w,

			IngestJobArgs{Site: "default", RecordID: "rec-remote-cancel-nil"},
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
			MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			Pool:          pool,
			RecordTarget:  pgstore.NewRecordings(pool),
		}
		recordingID := insertTestRecording(t, pool)
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
	var deletes atomic.Int32
	srv := newInstrumentedIngestServer(t, tsData, "test/followup-enqueue-failure.m2ts", func() { deletes.Add(1) })
	recordingID := insertTestRecording(t, pool)
	const recordID = "rec-followup-enqueue-failure"
	insertTestRecordSync(t, pool, recordingID, recordID)

	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	worker := &IngestWorker{
		Enqueuer:      &jobq.Deferred{},
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		Pool:          pool,
		RecordTarget:  pgstore.NewRecordings(pool),
		MediaDir:      t.TempDir(),
	}
	worker.CMDetectionStore = pgstore.NewAnalysis(pool)
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
	if got := deletes.Load(); got != 1 {
		t.Errorf("DeleteRecord calls = %d, want 1 after best-effort enqueue failures", got)
	}
}
