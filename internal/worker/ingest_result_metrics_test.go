package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// このファイルは IngestWorker.Work が result をどう決めるか（ジョブの結末の
// 分類）を固定する。
//
// 3 つの経路を分けて数えるのが目的である。
//
//   - graceful stop（River の soft stop）: 数えない。中断はジョブの結末ではない
//     --- River は attempt を消費せず行を available に戻し、次のプロセスが
//     再開して、そこで結末を 1 回だけ数える。
//   - 取り消された録画: canceled。
//   - 本当の失敗: failure。
//
// 分類の判定は Work の defer にあり、River 側の判定
// （internal/jobexecutor/job_executor.go の isSoftStopCancelError と、157 行目の
// res.Err の置き換え）と同じ材料（context.Cause(ctx) と返す err）を使う。

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

// writeRecordingRecord は mirakc の GET /records/{id} の応答を書く。
func writeRecordingRecord(w http.ResponseWriter, status, contentPath string) {
	record := mirakc.Record{
		Recording: mirakc.RecordInfo{Status: status, Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
		Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(record)
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

// TestIngestWorker_GracefulStopIsNotCounted は、River の soft stop で打ち切られた
// ingest が result の 3 値のどれも Inc せず、IngestDuration にも Observe しない
// ことを、実 River client + 実 DB で固定する。
//
// **実 River を使う理由:** 中断の判定材料（work ctx の cause が ErrStop であること・
// SoftStopTimeout が work ctx を WithoutCancel の子にする こと）は River の配線が
// 決める。合成した ctx では「本番でもそうなる」ことの根拠にならない。
//
// b2（blocking-records-get）が検出するのは **カウンタ側**であって River の行では
// ない。b2 が踏む経路（net/http が返す context.Cause(ctx)）は context.Canceled を
// 包まないので、`errors.Is(err, cause)` の項を消すと b2 だけが落ちる。
func TestIngestWorker_GracefulStopIsNotCounted(t *testing.T) {
	t.Run("following", func(t *testing.T) {
		started := make(chan struct{})
		var once sync.Once
		var recordGets atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
				// 追い付いた状態のまま。record は recording なので Work は追従を続ける。
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
				if recordGets.Add(1) >= 2 {
					// 1 回目は determineRelPath、2 回目は転送ループの status
					// ポーリング。2 回目＝Work が追従ループに入った合図。
					once.Do(func() { close(started) })
				}
				writeRecordingRecord(w, "recording", "test/graceful-follow.m2ts")
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)

		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		runGracefulStopIngest(t, pool, srv, "rec-graceful-follow", started)
	})

	t.Run("blocking-records-get", func(t *testing.T) {
		started := make(chan struct{})
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/") {
				// 最初の GetRecord は determineRelPath。応答を書かずに ctx の
				// 取り消しまで待つと、クライアント側が受け取るのは ctx.Err()
				// ではなく context.Cause(ctx)（= River の ErrStop）である
				// （net/http の transport.go が cancelRequest で cause を返す）。
				once.Do(func() { close(started) })
				<-r.Context().Done()
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		runGracefulStopIngest(t, pool, srv, "rec-graceful-block", started)
	})
}

func TestIngestWorker_SHA256WaitCompletesJobWithoutMetrics(t *testing.T) {
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
	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      mediaDir,
		StallTimeout:  time.Second,
		Pool:          pool,
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.PhysicalQueueName(jobs.IngestQueue, "default"): {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}
	events, cancelSubscribe := client.Subscribe(river.EventKindJobCompleted)
	defer cancelSubscribe()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	t.Cleanup(func() {
		clientCancel()
		<-client.Stopped()
	})

	beforeResults := ingestJobResults()
	durationBefore := ingestDurationSamples(t)
	inserted, err := client.Insert(context.Background(), jobs.IngestJobArgs{Site: "default", RecordID: recordID}, nil)
	if err != nil {
		t.Fatalf("inserting ingest job: %v", err)
	}
	var event *river.Event
	select {
	case event = <-events:
	case <-time.After(30 * time.Second):
		t.Fatal("ingest job did not complete while content.sha256 remained null")
	}
	if event.Job.ID != inserted.Job.ID {
		t.Errorf("completed job id = %d, want %d", event.Job.ID, inserted.Job.ID)
	}
	if event.Job.State != rivertype.JobStateCompleted {
		t.Fatalf("job state = %q, want completed while awaiting SHA-256", event.Job.State)
	}
	assertIngestResultDeltas(t, beforeResults, nil, "SHA-256 pending completion")
	if got := ingestDurationSamples(t); got != durationBefore {
		t.Errorf("IngestDuration samples after pending completion = %d, want %d", got, durationBefore)
	}

	var state string
	if err := pool.QueryRow(context.Background(), "SELECT state FROM river_job WHERE id = $1", inserted.Job.ID).Scan(&state); err != nil {
		t.Fatalf("reading river_job: %v", err)
	}
	if state != string(rivertype.JobStateCompleted) {
		t.Errorf("persisted river_job state = %q, want completed", state)
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

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("client.Stop: %v", err)
	}
	<-client.Stopped()
}

// runGracefulStopIngest は ingest ジョブを 1 件走らせ、ハンドラが started を閉じた
// ところで client.Stop をかける。停止後に「result の 3 値も IngestDuration の観測数も
// 動いていないこと」と「River が attempt を消費していないこと」を確認する。
func runGracefulStopIngest(t *testing.T, pool *pgxpool.Pool, srv *httptest.Server, recordID string, started <-chan struct{}) {
	t.Helper()

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		Pool:          pool,
	}
	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)

	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.PhysicalQueueName(jobs.IngestQueue, "default"): {MaxWorkers: 1},
		},
		Workers: workers,
		// 既定（5 秒）では停止が遅いだけなので縮める。値そのものは何も主張しない。
		SoftStopTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}

	inserted, err := client.Insert(context.Background(), jobs.IngestJobArgs{Site: "default", RecordID: recordID}, nil)
	if err != nil {
		t.Fatalf("inserting ingest job: %v", err)
	}

	before := ingestJobResults()
	durationBefore := ingestDurationSamples(t)

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("ingest が停止させる地点に到達しない")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("client.Stop: %v", err)
	}
	<-client.Stopped()

	assertIngestResultDeltas(t, before, nil, "graceful stop")
	if got := ingestDurationSamples(t); got != durationBefore {
		t.Errorf("IngestDuration の観測数 = %d, want %d（中断した試行は所要時間ではない）", got, durationBefore)
	}

	var state string
	var attempt int
	if err := pool.QueryRow(context.Background(),
		"SELECT state, attempt FROM river_job WHERE id = $1", inserted.Job.ID,
	).Scan(&state, &attempt); err != nil {
		t.Fatalf("reading river_job: %v", err)
	}
	if state != "available" || attempt != 0 {
		t.Errorf("river_job state=%q attempt=%d, want available / 0（soft stop は attempt を消費せず行を available に戻す）", state, attempt)
	}
}

// TestIngestWorker_RemoteCancelIsCountedAsCanceled は、River のリモート取消
// （Client.JobCancel が work ctx を rivertype.ErrJobCancelledRemotely で取り消す）
// で終わったジョブの result を固定する。
//
// **実 River は使わない。** rokuban に JobCancel の呼び出し元は無く、River の
// 配線ごと動かすと検証したい分岐の外側（producer / completer）に依存する。
// ここで見るのは Work の defer が cause から何を決めるかである。
//
// ケースが 2 つあるのは、River が **err != nil のときだけ**リモート取消を
// cancelled にし、nil のときは completed にするため（job_executor.go の 157 行目は
// res.Err != nil のときにしか cause で置き換えない）。Work 側も同じ向きに揃える。
func TestIngestWorker_RemoteCancelIsCountedAsCanceled(t *testing.T) {
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
		}
		recordingID := insertTestRecording(t, pool)
		insertTestRecordSync(t, pool, recordingID, "rec-remote-cancel-err")

		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(river.ErrJobCancelledRemotely)

		before := ingestJobResults()
		err := w.Work(ctx, &river.Job[IngestJobArgs]{
			JobRow: &rivertype.JobRow{ID: 425020},
			Args:   IngestJobArgs{Site: "default", RecordID: "rec-remote-cancel-err"},
		})
		if err == nil {
			t.Fatal("Work() = nil, want an error（ctx を取り消した状態では最初の DB 取得で失敗する）")
		}
		assertIngestResultDeltas(t, before, map[string]float64{"canceled": 1}, "remote cancel（Work が err を返す）")
	})

	t.Run("Work returns nil", func(t *testing.T) {
		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 原本が既にあるので Work は転送せず handleAlreadyCommittedIngest へ入る。
			// その最初の HTTP 要求（DeleteRecord）で取り消すと、削除の失敗はログだけ
			// なので Work は nil を返す。
			cancel(river.ErrJobCancelledRemotely)
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		w := &IngestWorker{
			MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
			MediaDir:      t.TempDir(),
			StallTimeout:  time.Second,
			Pool:          pool,
		}
		recordingID := insertTestRecording(t, pool)
		insertTestRecordSync(t, pool, recordingID, "rec-remote-cancel-nil")
		insertTestOriginalMediaAsset(t, pool, recordingID)

		before := ingestJobResults()
		err := w.Work(ctx, &river.Job[IngestJobArgs]{
			JobRow: &rivertype.JobRow{ID: 425021},
			Args:   IngestJobArgs{Site: "default", RecordID: "rec-remote-cancel-nil"},
		})
		if err != nil {
			t.Fatalf("Work() = %v, want nil", err)
		}
		assertIngestResultDeltas(t, before, map[string]float64{"success": 1}, "remote cancel（Work が nil を返す）")
	})
}
