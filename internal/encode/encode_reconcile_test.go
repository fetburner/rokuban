package encode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// encodeConfigWith は名前だけが意味を持つ encode 設定を作る（このパスは
// プロファイル名しか見ない --- ffmpeg は起動しない）。
func encodeConfigWith(names ...string) config.EncodeConfig {
	profiles := make([]config.EncodeProfile, 0, len(names))
	for _, n := range names {
		profiles = append(profiles, config.EncodeProfile{
			Name: n, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		})
	}
	return config.EncodeConfig{Profiles: profiles}
}

// runEncodeReconcilePass runs one pass and records the Args it asks the queue to deliver.
func runEncodeReconcilePass(t *testing.T, w *EncodeReconcileWorker) *jobqfaketest.RecordingEnqueuer {
	return runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
}

func runEncodeReconcilePassArgs(t *testing.T, w *EncodeReconcileWorker, args jobs.EncodeReconcileArgs) *jobqfaketest.RecordingEnqueuer {
	t.Helper()
	if w.Media == nil {
		t.Fatal("EncodeReconcileWorker.Media is nil")
	}
	enqueuer, ok := w.Enqueuer.(*jobqfaketest.RecordingEnqueuer)
	if !ok {
		enqueuer = jobqfaketest.NewRecordingEnqueuer()
		w.Enqueuer = enqueuer
	}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, args), "EncodeReconcileWorker.Work")
	return enqueuer
}

func findEncodeJob(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, recordingID int64, profile string) jobs.EncodeJobArgs {
	t.Helper()
	for _, args := range recordedArgs[jobs.EncodeJobArgs](t, enqueuer.CallsOfKind(jobs.EncodeJobArgs{}.Kind())) {
		if args.RecordingID == recordingID && args.Profile == profile {
			return args
		}
	}
	t.Fatalf("no encode job for recording %d profile %q", recordingID, profile)
	return jobs.EncodeJobArgs{}
}

func TestEncodeReconcile_TargetedPassOnlyQueuesSelectedRecordingAndLeavesFullStateAlone(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "targeted/first.m2ts", []string{"h265"}, []byte("x"))
	target := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "targeted/target.m2ts", []string{"h265"}, []byte("x"))
	third := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "targeted/third.m2ts", []string{"h265"}, []byte("x"))

	metrics.EncodeReconcileCandidates.Set(17)
	metrics.EncodeReconcileUnsatisfiable.WithLabelValues("removed-profile").Set(31)
	metrics.CutAwaitingReview.WithLabelValues("cut").Set(37)
	t.Cleanup(func() {
		metrics.EncodeReconcileCandidates.Set(0)
		metrics.EncodeReconcileUnsatisfiable.WithLabelValues("removed-profile").Set(0)
		metrics.CutAwaitingReview.WithLabelValues("cut").Set(0)
	})

	markers := pgstoretest.Reservations(pool)
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), RowLimit: 1, Media: pgstoretest.Media(pool), Markers: markers}
	runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{RecordingID: target})

	for _, tc := range []struct {
		id   int64
		want int
	}{{first, 0}, {target, 1}, {third, 0}} {
		if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), tc.id, "h265"); got != tc.want {
			t.Errorf("encode jobs for recording %d = %d, want %d", tc.id, got, tc.want)
		}
	}
	if got := len(recordedArgs[jobs.EncodeReconcileArgs](t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer).CallsOfKind("encode_reconcile"))); got != 0 {
		t.Errorf("targeted pass continuation jobs = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 17 {
		t.Errorf("candidate gauge = %v, want unchanged 17", got)
	}
	// 対象パスは候補を見ただけで、全件パスの完走（loop_success_markers の encode）を記録しない。
	saved, err := markers.ReadLoopMarkers(context.Background(), store.LoopReconcilePass)
	testutil.RequireNoError(t, err, "ReadLoopMarkers")
	if at, ok := saved[store.ReconcilePassEncode]; ok {
		t.Errorf("targeted pass recorded a full encode pass at %v, want none", at)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("removed-profile")); got != 31 {
		t.Errorf("unsatisfiable gauge = %v, want unchanged 31", got)
	}
	if got := promtestutil.ToFloat64(metrics.CutAwaitingReview.WithLabelValues("cut")); got != 37 {
		t.Errorf("awaiting-review gauge = %v, want unchanged 37", got)
	}
}

// 転送待ちが storage.transfer.backlog_limit に達した置き場へは encode を積まず、上限に届かなければ積む（背圧）。
// 原本のコピーは edge にだけあり desired は central なので、この録画は edge の転送待ちになる。
func TestEncodeReconcile_TransferBacklogStopsEncodesIntoTheSourceStore(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = "backpressure/original.ts"
		params.SizeBytes = 1
	})
	// dbtest.MediaAsset はコピーを test-store に作る。その原本のコピーを edge の置き場へ移す。
	testutil.MustExec(t, pool, "moving original copy to edge", `UPDATE media_asset_copies SET store = 'edge' WHERE media_asset_id = $1`, assetID)
	seedEncodePolicy(t, pool, recordingID, []string{"h265"})

	for _, tc := range []struct {
		name     string
		limit    int
		wantJobs int
		wantGate float64
	}{
		{name: "limit 1 stops the encode", limit: 1, wantJobs: 0, wantGate: 1},
		{name: "limit 2 allows the encode", limit: 2, wantJobs: 1, wantGate: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := centralTransferTestConfig()
			policy.Transfer.BacklogLimit = map[string]int{"edge": tc.limit}
			transfers := &countingTransfers{StorageTransfer: pgstoretest.Media(pool)}
			w := &EncodeReconcileWorker{
				Profiles:      encodeConfigWith("h265"),
				Media:         pgstoretest.Media(pool),
				Transfers:     transfers,
				StoragePolicy: policy,
			}
			enqueuer := runEncodeReconcilePass(t, w)
			if transfers.attemptLimit != 10 {
				t.Fatalf("backlog count attempt limit = %d, want 10", transfers.attemptLimit)
			}
			if got := countEncodeJobs(t, enqueuer, recordingID, "h265"); got != tc.wantJobs {
				t.Fatalf("encode jobs for recording %d = %d, want %d", recordingID, got, tc.wantJobs)
			}
			if tc.wantJobs == 1 {
				if got := findEncodeJob(t, enqueuer, recordingID, "h265").Store; got != "edge" {
					t.Fatalf("encode job store = %q, want edge", got)
				}
			}
			// 件数は上限の両側で同じ値（1）を出す。止める判断と表示が同じ件数を見ていることの確認。
			gauges := scrapeBacklogGauges(t, pool, policy)
			if got := gauges["rokuban_encode_backpressure"]; got != tc.wantGate {
				t.Fatalf("backpressure gauge for edge = %v, want %v", got, tc.wantGate)
			}
			if got := gauges["rokuban_storage_transfer_backlog_assets"]; got != 1 {
				t.Fatalf("transfer backlog gauge for edge = %v, want 1", got)
			}
		})
	}
}

// countingTransfers は転送待ちの集計が呼ばれた回数を数える。問い合わせを省いたパスを確かめるために使う。
type countingTransfers struct {
	store.StorageTransfer
	calls, reads, records int
	attemptLimit          int32
}

func (c *countingTransfers) ReadTransferBacklog(ctx context.Context) (map[string]store.TransferBacklogSnapshot, error) {
	c.reads++
	return c.StorageTransfer.ReadTransferBacklog(ctx)
}

func (c *countingTransfers) RecordTransferBacklog(ctx context.Context, counts map[string]int64, countedAt time.Time) (map[string]store.TransferBacklogWrite, error) {
	c.records++
	return c.StorageTransfer.RecordTransferBacklog(ctx, counts, countedAt)
}

func (c *countingTransfers) CountMediaAssetTransferBacklogBySourceStore(ctx context.Context, originalRules []byte, fsStores, sourceStores []string, derivedStore string, attemptLimit int32) (map[string]int64, error) {
	c.calls++
	c.attemptLimit = attemptLimit
	return c.StorageTransfer.CountMediaAssetTransferBacklogBySourceStore(ctx, originalRules, fsStores, sourceStores, derivedStore, attemptLimit)
}

// seedEdgeOriginal は原本のコピーだけを edge に置いた録画を作る（desired は central なので edge の転送待ち）。
func seedEdgeOriginal(t *testing.T, pool *pgxpool.Pool) (recordingID, assetID int64) {
	t.Helper()
	recordingID = dbtest.Recording(t, pool)
	assetID = dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
		params.RelPath = "backlog/" + t.Name() + ".ts"
		params.SizeBytes = 1
	})
	testutil.MustExec(t, pool, "moving original copy to edge", `UPDATE media_asset_copies SET store = 'edge' WHERE media_asset_id = $1`, assetID)
	seedEncodePolicy(t, pool, recordingID, []string{"h265"})
	return recordingID, assetID
}

// 転送が済んで原本が central にだけ残った状態にする（転送待ちは 0 件になる）。
func moveOriginalToCentral(t *testing.T, pool *pgxpool.Pool, assetID int64) {
	t.Helper()
	testutil.MustExec(t, pool, "moving original copy to central", `UPDATE media_asset_copies SET store = 'central' WHERE media_asset_id = $1`, assetID)
}

// scrapeBacklogGauges は、DB から取り直す collector を通して edge の 2 本のゲージを読む。
// 系列が無い（まだ数えていない）ときは map に入らない。
func scrapeBacklogGauges(t *testing.T, pool *pgxpool.Pool, policy config.StorageConfig) map[string]float64 {
	t.Helper()
	limits := make(map[string]int)
	for name := range policy.Transfer.BacklogLimit {
		limits[name] = policy.TransferBacklogLimit(name)
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(metrics.NewTransferBacklogCollector(pgstoretest.Media(pool), limits))
	families, err := reg.Gather()
	testutil.RequireNoError(t, err, "gathering backlog gauges")
	out := make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "store" && label.GetValue() == "edge" {
					out[family.GetName()] = metric.Gauge.GetValue()
				}
			}
		}
	}
	return out
}

// 止まっている置き場は、次の targeted pass で件数を数え直し、解けたらゲージと投入の両方に出る。
// パスごとに新しい worker を作る（ScaledJob の --once はパスごとにプロセスが新しくなる）。
func TestEncodeReconcile_BacklogGaugeClearsOnTargetedPass(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	recordingID, assetID := seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 1}
	newWorker := func() *EncodeReconcileWorker {
		return &EncodeReconcileWorker{
			Profiles:      encodeConfigWith("h265"),
			Media:         pgstoretest.Media(pool),
			Transfers:     pgstoretest.Media(pool),
			StoragePolicy: policy,
		}
	}

	runEncodeReconcilePassArgs(t, newWorker(), jobs.EncodeReconcileArgs{})
	gauges := scrapeBacklogGauges(t, pool, policy)
	if gauges["rokuban_encode_backpressure"] != 1 || gauges["rokuban_storage_transfer_backlog_assets"] != 1 {
		t.Fatalf("after full pass, gauges = %v, want backpressure 1 and backlog 1", gauges)
	}

	moveOriginalToCentral(t, pool, assetID)
	enqueuer := runEncodeReconcilePassArgs(t, newWorker(), jobs.EncodeReconcileArgs{RecordingID: recordingID})
	gauges = scrapeBacklogGauges(t, pool, policy)
	if gauges["rokuban_encode_backpressure"] != 0 || gauges["rokuban_storage_transfer_backlog_assets"] != 0 {
		t.Fatalf("after backlog cleared and targeted pass, gauges = %v, want backpressure 0 and backlog 0", gauges)
	}
	if got := countEncodeJobs(t, enqueuer, recordingID, "h265"); got != 1 {
		t.Fatalf("encode jobs after release = %d, want 1", got)
	}
}

// 設定から上限を外した置き場の行は、次のパスが消し、ゲージにも出ない。
func TestEncodeReconcile_BacklogSnapshotDroppedWhenLimitRemoved(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 1}
	newWorker := func(p config.StorageConfig) *EncodeReconcileWorker {
		return &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: pgstoretest.Media(pool), StoragePolicy: p}
	}
	runEncodeReconcilePassArgs(t, newWorker(policy), jobs.EncodeReconcileArgs{})
	if got, _ := pgstoretest.Media(pool).ReadTransferBacklog(t.Context()); got["edge"].Assets != 1 {
		t.Fatalf("saved backlog = %v, want edge 1", got)
	}

	policy.Transfer.BacklogLimit = nil
	runEncodeReconcilePassArgs(t, newWorker(policy), jobs.EncodeReconcileArgs{})
	if got, _ := pgstoretest.Media(pool).ReadTransferBacklog(t.Context()); len(got) != 0 {
		t.Fatalf("saved backlog after limit removed = %v, want none", got)
	}
}

// 候補の原本が上限の無い置き場にあり、止まっている置き場も無いパスは集計を問い合わせない。
// 候補が上限付きの置き場にあるパスは、止めるかどうかを決めるために数える。
func TestEncodeReconcile_PassWithoutDecisionSkipsBacklogCount(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 1}

	t.Run("original on an unlimited store is not counted", func(t *testing.T) {
		recordingID, assetID := seedEdgeOriginal(t, pool)
		moveOriginalToCentral(t, pool, assetID)
		counting := &countingTransfers{StorageTransfer: pgstoretest.Media(pool)}
		w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: counting, StoragePolicy: policy}
		enqueuer := runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{RecordingID: recordingID})
		if counting.calls != 0 {
			t.Fatalf("backlog counts = %d, want 0", counting.calls)
		}
		if got := countEncodeJobs(t, enqueuer, recordingID, "h265"); got != 1 {
			t.Fatalf("encode jobs = %d, want 1", got)
		}
	})

	t.Run("original on a limited store is counted and stops the encode", func(t *testing.T) {
		recordingID, _ := seedEdgeOriginal(t, pool)
		counting := &countingTransfers{StorageTransfer: pgstoretest.Media(pool)}
		w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: counting, StoragePolicy: policy}
		enqueuer := runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{RecordingID: recordingID})
		if counting.calls != 1 {
			t.Fatalf("backlog counts = %d, want 1", counting.calls)
		}
		if got := countEncodeJobs(t, enqueuer, recordingID, "h265"); got != 0 {
			t.Fatalf("encode jobs = %d, want 0", got)
		}
	})
}

// 上限を設定していない構成の targeted pass は、DB の件数にも保存にも集計にも触らない。
// 全件パスの先頭だけ、残った行の掃除のために保存を呼ぶ。
func TestEncodeReconcile_NoLimitPassDoesNotTouchBacklogState(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	recordingID, _ := seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	counting := &countingTransfers{StorageTransfer: pgstoretest.Media(pool)}
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: counting, StoragePolicy: policy}

	runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{RecordingID: recordingID})
	if counting.calls != 0 || counting.reads != 0 || counting.records != 0 {
		t.Fatalf("targeted pass without limits: counts=%d reads=%d records=%d, want all 0", counting.calls, counting.reads, counting.records)
	}
	runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
	if counting.calls != 0 || counting.reads != 0 || counting.records != 1 {
		t.Fatalf("full pass without limits: counts=%d reads=%d records=%d, want 0, 0, 1", counting.calls, counting.reads, counting.records)
	}
}

// newerCountTransfers は、worker が数えて保存するまでの間に、別のパスがより新しい件数を保存した状況を作る。
type newerCountTransfers struct {
	store.StorageTransfer
	newer map[string]int64
}

func (n *newerCountTransfers) RecordTransferBacklog(ctx context.Context, counts map[string]int64, countedAt time.Time) (map[string]store.TransferBacklogWrite, error) {
	if _, err := n.StorageTransfer.RecordTransferBacklog(ctx, n.newer, time.Now().Add(time.Minute)); err != nil {
		return nil, err
	}
	return n.StorageTransfer.RecordTransferBacklog(ctx, counts, countedAt)
}

// 自分の件数が古くて捨てられたときは、DB に残った新しい件数で止める判断をし、Warn / Info は出さない。
// 自分の数えた件数は 1（上限 2 の手前）だが、より新しい件数が 2（上限）なので、encode は投入しない。
func TestEncodeReconcile_StaleCountDecidesByTheSavedCount(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	logs := captureSlog(t)
	recordingID, _ := seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 2}
	w := &EncodeReconcileWorker{
		Profiles:      encodeConfigWith("h265"),
		Media:         pgstoretest.Media(pool),
		Transfers:     &newerCountTransfers{StorageTransfer: pgstoretest.Media(pool), newer: map[string]int64{"edge": 2}},
		StoragePolicy: policy,
	}
	enqueuer := runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
	if got := countEncodeJobs(t, enqueuer, recordingID, "h265"); got != 0 {
		t.Fatalf("encode jobs = %d, want 0 (the saved newer count stops the store)", got)
	}
	if gauges := scrapeBacklogGauges(t, pool, policy); gauges["rokuban_storage_transfer_backlog_assets"] != 2 || gauges["rokuban_encode_backpressure"] != 1 {
		t.Fatalf("gauges = %v, want backlog 2 and backpressure 1", gauges)
	}
	// 新しい件数を書いたのは別のパスなので、この呼び出しは Warn も Info も出さない。
	if strings.Contains(logs.String(), backlogStopMsg) || strings.Contains(logs.String(), backlogResumeMsg) {
		t.Fatalf("stale pass logged a transition\n%s", logs.String())
	}
}

// 保存した件数を失っても（導出表なので作り直せる）、次に数えたパスが作り直す。
// 止まっていた置き場は Warn がもう 1 回出て、数えるまでその置き場の系列は出ない。
func TestEncodeReconcile_LostSnapshotIsRebuiltAndWarnsAgain(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	logs := captureSlog(t)
	seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 1}
	pass := func() {
		w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: pgstoretest.Media(pool), StoragePolicy: policy}
		runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
	}
	pass()
	testutil.MustExec(t, pool, "losing snapshots", `DELETE FROM transfer_backlog_snapshots`)
	if gauges := scrapeBacklogGauges(t, pool, policy); len(gauges) != 0 {
		t.Fatalf("gauges after losing snapshots = %v, want none", gauges)
	}
	pass()
	if got := strings.Count(logs.String(), backlogStopMsg); got != 2 {
		t.Fatalf("stop logs after losing and rebuilding = %d, want 2\n%s", got, logs.String())
	}
	if gauges := scrapeBacklogGauges(t, pool, policy); gauges["rokuban_encode_backpressure"] != 1 {
		t.Fatalf("gauges after rebuilding = %v, want backpressure 1", gauges)
	}
}

// clockTransfers は DB の時計を固定値にし、保存に渡された countedAt を記録する。
type clockTransfers struct {
	store.StorageTransfer
	clock    time.Time
	recorded time.Time
}

func (c *clockTransfers) DatabaseClock(context.Context) (time.Time, error) { return c.clock, nil }

func (c *clockTransfers) RecordTransferBacklog(ctx context.Context, counts map[string]int64, countedAt time.Time) (map[string]store.TransferBacklogWrite, error) {
	c.recorded = countedAt
	return c.StorageTransfer.RecordTransferBacklog(ctx, counts, countedAt)
}

// 保存する countedAt は、プロセスの時計ではなく DB の時計から取った値になる。
func TestEncodeReconcile_CountedAtComesFromTheDatabaseClock(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 1}
	clock := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	transfers := &clockTransfers{StorageTransfer: pgstoretest.Media(pool), clock: clock}
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: transfers, StoragePolicy: policy}
	runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
	if !transfers.recorded.Equal(clock) {
		t.Fatalf("recorded countedAt = %v, want the database clock %v", transfers.recorded, clock)
	}
}

// 止まり始めたときと解けたときだけ Warn / Info を出す。止まっている間は毎パス同じ Warn を出さない。
// パスごとに新しい worker を作る（ScaledJob の --once はパスごとにプロセスが新しくなる）。
func TestEncodeReconcile_BacklogLogsOnlyOnTransitions(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	logs := captureSlog(t)
	_, assetID := seedEdgeOriginal(t, pool)
	policy := centralTransferTestConfig()
	policy.Transfer.BacklogLimit = map[string]int{"edge": 1}
	pass := func() {
		w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Transfers: pgstoretest.Media(pool), StoragePolicy: policy}
		runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
	}

	pass()
	pass()
	if got := strings.Count(logs.String(), backlogStopMsg); got != 1 {
		t.Fatalf("stop logs after two passes while blocked = %d, want 1\n%s", got, logs.String())
	}

	moveOriginalToCentral(t, pool, assetID)
	pass()
	if got := strings.Count(logs.String(), backlogResumeMsg); got != 1 {
		t.Fatalf("resume logs after release = %d, want 1\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), backlogStopMsg); got != 1 {
		t.Fatalf("stop logs after release = %d, want still 1", got)
	}
	pass()
	if got := strings.Count(logs.String(), backlogResumeMsg); got != 1 {
		t.Fatalf("resume logs after a further pass = %d, want still 1", got)
	}
}

const (
	backlogStopMsg   = `msg="encode_reconcile: transfer backlog reached the limit`
	backlogResumeMsg = `msg="encode_reconcile: transfer backlog is below the limit`
)

// captureSlog は slog の既定の出力をテストの間だけバッファへ向ける。
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestEncodeReconcileReportAwaitingReviewUsesSharedPredicate は、worker が確認待ちのゲージを
// store の ListCutAwaitingReview の結果から、設定の cut profile だけについて作ることを固定する。
//
// 述語そのもの（ごみ箱・原本・所有・試行・作り直しの除外）と、ListCutAwaitingReview と
// ListAwaitingReviewRecordingIDs の一致は contracttest（runAwaitingReviewRecordingIDs）が
// 方言ごとに見る。ここに残すのは worker の配線だけ。設定に無い profile（removed）は
// CutProfileNames から外れるのでラベルが立たない。
func TestEncodeReconcileReportAwaitingReviewUsesSharedPredicate(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	metrics.CutAwaitingReview.Reset()
	t.Cleanup(metrics.CutAwaitingReview.Reset)

	newRecording := func(title string, profiles []string) {
		t.Helper()
		id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Title = title })
		seedEncodePolicy(t, pool, id, profiles)
		dbtest.MediaAsset(t, pool, id)
	}

	newRecording("one cut", []string{"cut"})
	newRecording("two cuts", []string{"cut", "cut2"})
	newRecording("removed cut profile", []string{"removed"})

	profiles := encodeConfigWith("cut", "cut2")
	for i := range profiles.Profiles {
		profiles.Profiles[i].Cut = true
	}
	worker := &EncodeReconcileWorker{Media: pgstoretest.Media(pool), Profiles: profiles}
	worker.reportAwaitingReview(ctx)
	if got := promtestutil.ToFloat64(metrics.CutAwaitingReview.WithLabelValues("cut")); got != 2 {
		t.Errorf("cut awaiting-review gauge = %v, want 2", got)
	}
	if got := promtestutil.ToFloat64(metrics.CutAwaitingReview.WithLabelValues("cut2")); got != 1 {
		t.Errorf("cut2 awaiting-review gauge = %v, want 1", got)
	}
}

// TestEncodeReconcile_QueuesUnsatisfiedRebuildsAndKeepsEveryRow は、reconcile が
// 満たされていない作り直しだけに押した時点の keep のジョブを投入し、どの要求行も
// 消さないことを固定する。チャプターの編集・所有の欠落・満たされた状態のどれでも
// 行は残る（消すのは api だけ）。
func TestEncodeReconcile_QueuesUnsatisfiedRebuildsAndKeepsEveryRow(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	staleFrozen := []chapters.Range{{StartMs: 0, EndMs: 1800000}}
	seedCut := func(name string, frozen []chapters.Range) (int64, []chapters.Range, int64) {
		t.Helper()
		id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, name, []string{"cut"}, []byte("original"))
		dbtest.AdoptChapters(t, pool, id)
		dbtest.ChapterSpan(t, pool, id)
		keep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, id)
		if err != nil || !owned || len(keep) == 0 {
			t.Fatalf("currentCutKeep(%d) = %v, owned %v, err %v", id, keep, owned, err)
		}
		assetID := seedEncodedAsset(t, pool, id, "cut", name+"_cut.mp4")
		if err := setFrozenCuts(t, pool, assetID, frozen); err != nil {
			t.Fatalf("seeding frozen cut for %d: %v", id, err)
		}
		return id, keep, assetID
	}
	unsatisfied, unsatisfiedKeep, _ := seedCut("rebuild/unsatisfied.m2ts", staleFrozen)
	seedEncodedAssetRebuildRequest(t, pool, unsatisfied, unsatisfiedKeep)

	// 押した後にチャプターを編集した（requested_keep は今のチャプターと違う）。
	edited, _, _ := seedCut("rebuild/edited.m2ts", staleFrozen)
	editedRequested := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	seedEncodedAssetRebuildRequest(t, pool, edited, editedRequested)

	// 所有の行が無い。押した時点の keep が確認なので、所有は投入の条件にしない。
	unowned, unownedKeep, _ := seedCut("rebuild/unowned.m2ts", staleFrozen)
	seedEncodedAssetRebuildRequest(t, pool, unowned, unownedKeep)
	testutil.MustExec(t, pool, "resetting chapter ownership", `DELETE FROM recording_chapter_ownership WHERE recording_id = $1`, unowned)

	// 凍結区間が requested_keep と等しい = 満たされている。
	satisfied, satisfiedKeep, assetID := seedCut("rebuild/satisfied.m2ts", staleFrozen)
	testutil.RequireNoError(t, setFrozenCuts(t, pool, assetID, satisfiedKeep), "freezing satisfied cut")
	seedEncodedAssetRebuildRequest(t, pool, satisfied, satisfiedKeep)

	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}}, Media: pgstoretest.Media(pool)}
	enqueuer := runEncodeReconcilePass(t, w)

	wantKeep := map[int64][]chapters.Range{unsatisfied: unsatisfiedKeep, edited: editedRequested, unowned: unownedKeep}
	for id, want := range wantKeep {
		if got := countEncodeJobs(t, enqueuer, id, "cut"); got != 1 {
			t.Fatalf("encode jobs for recording %d = %d, want 1", id, got)
		}
		for _, call := range recordedArgs[jobs.EncodeJobArgs](t, enqueuer.CallsOfKind(jobs.EncodeJobArgs{}.Kind())) {
			if call.RecordingID != id {
				continue
			}
			var queued []chapters.Range
			if err := json.Unmarshal(call.RequestedCutKeep, &queued); err != nil {
				t.Fatalf("decoding queued requested keep for %d: %v", id, err)
			}
			if !chapters.SameRanges(queued, want) {
				t.Errorf("recording %d queued keep = %v, want the requested %v", id, queued, want)
			}
		}
	}
	if got := countEncodeJobs(t, enqueuer, satisfied, "cut"); got != 0 {
		t.Errorf("encode jobs for satisfied request = %d, want 0", got)
	}
	for _, id := range []int64{unsatisfied, edited, unowned, satisfied} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("counting rebuild requests for %d: %v", id, err)
		}
		if count != 1 {
			t.Errorf("rebuild request rows for recording %d = %d, want 1 (only api removes them)", id, count)
		}
	}
}

func TestEncodeReconcile_PendingRebuildWithoutAssetQueuesOnlyBoundJob(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/missing-asset.m2ts", []string{"cut"}, []byte("original"))
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.ChapterSpan(t, pool, recordingID)
	keep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{
		Enqueuer: enqueuer,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}},
		Media:    pgstoretest.Media(pool),
	}
	runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{RecordingID: recordingID})

	var got []jobs.EncodeJobArgs
	for _, args := range recordedArgs[jobs.EncodeJobArgs](t, enqueuer.CallsOfKind(jobs.EncodeJobArgs{}.Kind())) {
		if args.RecordingID == recordingID && args.Profile == "cut" {
			got = append(got, args)
		}
	}
	if len(got) != 1 {
		t.Fatalf("queued cut jobs = %d, want exactly one request-bound job", len(got))
	}
	requestedKeep, present, err := decodeRequestedCutKeep(got[0].RequestedCutKeep)
	if err != nil || !present || !chapters.SameRanges(requestedKeep, keep) {
		t.Errorf("queued request keep = %v, present %v, err %v; want %v", requestedKeep, present, err, keep)
	}
}

func TestEncodeReconcile_RebuildsPrecedeMissingProfilesAndSortByProfile(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/order.m2ts", []string{"h264", "cut-b", "cut-a"}, []byte("x"))
	for profile, keep := range map[string][]chapters.Range{
		"cut-b": {{StartMs: 2000, EndMs: 3000}},
		"cut-a": {{StartMs: 1000, EndMs: 2000}},
	} {
		if err := sqlcgen.New(pool).UpsertEncodedAssetRebuildRequest(ctx, sqlcgen.UpsertEncodedAssetRebuildRequestParams{
			RecordingID:   recordingID,
			Profile:       profile,
			RequestedKeep: mustKeepRanges(t, keep),
		}); err != nil {
			t.Fatalf("seeding %s rebuild request: %v", profile, err)
		}
	}
	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{
		{Name: "cut-b", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
		{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
		{Name: "cut-a", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}
	w := &EncodeReconcileWorker{Profiles: profiles, RowLimit: 1, Media: pgstoretest.Media(pool)}
	enqueuer := runEncodeReconcilePass(t, w)
	got := recordedArgs[jobs.EncodeJobArgs](t, enqueuer.CallsOfKind(jobs.EncodeJobArgs{}.Kind()))
	want := []string{"cut-a", "cut-b", "h264"}
	if len(got) != len(want) {
		t.Fatalf("encode jobs = %+v, want profile order %v", got, want)
	}
	for i := range got {
		if got[i].RecordingID != recordingID || got[i].Profile != want[i] {
			t.Errorf("encode job %d = (%d, %q), want (%d, %q)", i, got[i].RecordingID, got[i].Profile, recordingID, want[i])
		}
	}
}

func mustKeepRanges(t *testing.T, keep []chapters.Range) pgstore.KeepRanges {
	t.Helper()
	ranges, err := pgstore.KeepRangesParam(keep)
	testutil.RequireNoError(t, err, "encoding requested keep ranges")
	return ranges
}

// TestEncodeReconcile_TargetedPassSkipsTrashedUntilRestoredFullPass は、対象パスが
// ごみ箱の録画を除外し、復元後の全件パスが不足分を投入することを固定する。
func TestEncodeReconcile_TargetedPassSkipsTrashedUntilRestoredFullPass(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "targeted-trash/original.m2ts", []string{"h265"}, []byte("x"))
	_, err := sqlcgen.New(pool).SoftDeleteRecording(ctx, recordingID)
	testutil.RequireNoError(t, err, "soft deleting recording")

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool)}
	runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{RecordingID: recordingID})
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Fatalf("targeted encode jobs for trashed recording = %d, want 0", got)
	}

	_, err = sqlcgen.New(pool).RestoreRecording(ctx, recordingID)
	testutil.RequireNoError(t, err, "restoring recording")
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID, "h265"); got != 1 {
		t.Fatalf("full-pass encode jobs after restore = %d, want 1", got)
	}
}

// TestEncodeReconcileWorker_BatchesCandidateReads は、reconcile パスが候補ごとの
// 原本・ポリシー・encoded の再取得をしないことを固定する。enqueue は不足
// プロファイルごとに残るが、読み取りは候補数に比例して増えない。
func TestEncodeReconcileWorker_BatchesCandidateReads(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "batch/1.m2ts", []string{"h264", "h265"}, []byte("x"))
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "batch/2.m2ts", []string{"h264", "h265"}, []byte("x"))

	counter := &encodeReconcileQueryCounter{}
	tracedPool := poolWithQueryTracer(t, pool, counter)
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h264", "h265"), RowLimit: 2, Media: pgstoretest.Media(tracedPool)}
	runEncodeReconcilePass(t, w)

	if got := counter.listMissing.Load(); got != 1 {
		t.Errorf("ListMissingEncodeProfiles calls = %d, want 1", got)
	}
	if got := counter.activeOriginal.Load(); got != 0 {
		t.Errorf("GetActiveOriginalMediaAsset calls = %d, want 0", got)
	}
	if got := counter.encodePolicy.Load(); got != 0 {
		t.Errorf("GetRecordingEncodePolicy calls = %d, want 0", got)
	}
	if got := counter.activeEncoded.Load(); got != 0 {
		t.Errorf("GetActiveEncodedMediaAssetID calls = %d, want 0", got)
	}
}

type encodeReconcileQueryCounter struct {
	listMissing    atomic.Int64
	activeOriginal atomic.Int64
	encodePolicy   atomic.Int64
	activeEncoded  atomic.Int64
}

func (c *encodeReconcileQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, "-- name: ListMissingEncodeProfiles"):
		c.listMissing.Add(1)
	case strings.Contains(data.SQL, "-- name: GetActiveOriginalMediaAsset"):
		c.activeOriginal.Add(1)
	case strings.Contains(data.SQL, "-- name: GetRecordingEncodePolicy"):
		c.encodePolicy.Add(1)
	case strings.Contains(data.SQL, "-- name: GetActiveEncodedMediaAssetID"):
		c.activeEncoded.Add(1)
	}
	return ctx
}

func (*encodeReconcileQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func poolWithQueryTracer(t *testing.T, pool *pgxpool.Pool, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), config)
	testutil.RequireNoError(t, err, "creating traced pool")
	t.Cleanup(traced.Close)
	return traced
}

// 候補が 0 件のパスでも成功すること（通常運転はこちら）。
func TestEncodeReconcileWorker_NoCandidates(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "none/a.m2ts", nil, []byte("x"))

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h264"), Media: pgstoretest.Media(pool)}
	runEncodeReconcilePass(t, w)

	if jobs := len(recordedArgs[jobs.EncodeJobArgs](t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer).CallsOfKind(jobs.EncodeJobArgs{}.Kind()))); jobs != 0 {
		t.Errorf("encode jobs = %d, want 0", jobs)
	}
}

// TestEncodeReconcileWorker_SkipsProfilesMissingFromConfig は、設定から消えた
// プロファイルを投入しないこと・それでも設定に残っているプロファイルは投入する
// ことを両方向で見る。
//
// 投入してしまうと EncodeWorker が `unknown encode profile` で 25 回失敗 →
// discarded になり、pendingJobStates に discarded が無いので次パスがまた
// 投入する（15 分ごとに永久）。候補としても窓を恒久的に占有する。
func TestEncodeReconcileWorker_SkipsProfilesMissingFromConfig(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	// 同じ録画が「設定に残っている h265」と「設定から消えた gone」の両方を
	// 凍結している。候補には挙がるが、投入されるのは h265 だけ。
	mixed := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cfg/mixed.m2ts",
		[]string{"gone", "h265"}, []byte("x"))
	// desired が消えたプロファイルだけの録画。候補にも挙がらない。
	onlyGone := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cfg/gone.m2ts",
		[]string{"gone"}, []byte("x"))

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool)}
	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), mixed, "h265"); got != 1 {
		t.Errorf("encode jobs for the configured profile = %d, want 1", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), mixed, "gone"); got != 0 {
		t.Errorf("encode jobs for the removed profile (same recording) = %d, want 0", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), onlyGone, "gone"); got != 0 {
		t.Errorf("encode jobs for the removed profile (only desired) = %d, want 0", got)
	}

	// 落としたことは黙らせない（プロファイル別の件数をゲージに出す）。
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("gone")); got != 2 {
		t.Errorf("unsatisfiable gauge for %q = %v, want 2", "gone", got)
	}
}

// TestEncodeReconcileWorker_RowLimitCapsWorkPerPass は #326 の判定基準 C2
// （コスト）を固定する: 候補が RowLimit を超えていても、1 パスが examine するのは
// ちょうど RowLimit 件までであること。
func TestEncodeReconcileWorker_RowLimitCapsWorkPerPass(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cap/1.m2ts", []string{"h265"}, []byte("x"))
	second := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cap/2.m2ts", []string{"cut"}, []byte("x"))
	third := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cap/3.m2ts", []string{"cut"}, []byte("x"))
	if first >= second || second >= third {
		t.Fatalf("expected ascending recording ids, got first=%d second=%d third=%d", first, second, third)
	}
	secondKeep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	thirdKeep := []chapters.Range{{StartMs: 1000, EndMs: 2000}}
	secondRequested := seedEncodedAssetRebuildRequest(t, pool, second, secondKeep)
	thirdRequested := seedEncodedAssetRebuildRequest(t, pool, third, thirdKeep)

	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{
		{Name: "h265", Container: "mp4", VideoCodec: "libx265", AudioCodec: "aac"},
		{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}
	markers := pgstoretest.Reservations(pool)
	w := &EncodeReconcileWorker{Profiles: profiles, RowLimit: 2, Media: pgstoretest.Media(pool), Markers: markers}
	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), first, "h265"); got != 1 {
		t.Errorf("encode jobs for first = %d, want 1", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), second, "cut"); got != 1 {
		t.Errorf("encode jobs for second rebuild = %d, want 1", got)
	}
	// 3 件目は RowLimit=2 を超えるのでこのパスでは examine されない。
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), third, "cut"); got != 0 {
		t.Errorf("encode jobs for third = %d, want 0 (a single pass must not examine more than RowLimit candidates)", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 2 {
		t.Errorf("candidates gauge = %v, want 2 (equal to RowLimit)", got)
	}
	continuations := recordedArgs[jobs.EncodeReconcileArgs](t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer).CallsOfKind("encode_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterRecordingID != second {
		t.Errorf("continuations = %+v, want one cursor after recording %d", continuations, second)
	}
	secondJob := findEncodeJob(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), second, "cut")
	if !bytes.Equal(secondJob.RequestedCutKeep, secondRequested) {
		t.Errorf("second rebuild keep = %s, want %s", secondJob.RequestedCutKeep, secondRequested)
	}
	if len(continuations) == 1 {
		next := &EncodeReconcileWorker{Enqueuer: w.Enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 2}
		testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), next, continuations[0]), "continuation after rebuild candidate")
		if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), third, "cut"); got != 1 {
			t.Errorf("encode jobs for rebuild behind cursor = %d, want 1", got)
		}
		thirdJob := findEncodeJob(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), third, "cut")
		if !bytes.Equal(thirdJob.RequestedCutKeep, thirdRequested) {
			t.Errorf("third rebuild keep = %s, want %s", thirdJob.RequestedCutKeep, thirdRequested)
		}
	}
	// 全件パスの完走は DB の印に残る。これが無いと、完走の記録の書き込みを消しても
	// パッケージ全体のテストが green のまま通り、停止したパスを検出できない。
	saved, err := markers.ReadLoopMarkers(context.Background(), store.LoopReconcilePass)
	testutil.RequireNoError(t, err, "ReadLoopMarkers")
	if _, ok := saved[store.ReconcilePassEncode]; !ok {
		t.Error("encode pass marker was not recorded; a stalled pass would be undetectable")
	}
}

func TestEncodeReconcileWorker_ExhaustedRebuildRequestReturnsAfterRetryRequest(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "retry-exhausted-rebuild/1.m2ts", []string{"cut"}, []byte("x"))
	assetID := seedEncodedAsset(t, pool, id, "cut", "retry-exhausted-rebuild/cut.mp4")
	testutil.RequireNoError(t, setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}), "seeding previous cut keep")
	requestedKeep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	requestedJSON := seedEncodedAssetRebuildRequest(t, pool, id, requestedKeep)
	testutil.MustExec(t, pool, "seeding an exhausted rebuild attempt", `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'cut', 'failed', now(), $2)`, id, jobs.EncodeAttemptLimit)

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{{
		Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
	}}}
	firstWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, firstWorker, jobs.EncodeReconcileArgs{}), "reconcile with an exhausted rebuild attempt")
	if got := countEncodeJobs(t, enqueuer, id, "cut"); got != 0 {
		t.Fatalf("encode jobs for exhausted rebuild candidate = %d, want 0", got)
	}

	deleted, err := sqlcgen.New(pool).DeleteFailedRecordingEncodeAttempts(ctx, sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: id,
		Profiles:    []string{"cut"},
	})
	testutil.RequireNoError(t, err, "resetting failed rebuild attempt")
	if deleted != 1 {
		t.Fatalf("failed rebuild attempts deleted = %d, want 1", deleted)
	}
	enqueuer.Reset()
	secondWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, secondWorker, jobs.EncodeReconcileArgs{}), "reconcile after retry request")
	if got := countEncodeJobs(t, enqueuer, id, "cut"); got != 1 {
		t.Fatalf("encode jobs after rebuild retry request = %d, want 1", got)
	}
	if got := findEncodeJob(t, enqueuer, id, "cut").RequestedCutKeep; !bytes.Equal(got, requestedJSON) {
		t.Errorf("requested keep after retry = %s, want %s", got, requestedJSON)
	}
}

// TestEncodeReconcileWorker_ContinuationSurvivesFreshWorker は、--once のように
// パスごとに worker を作り直しても、候補窓の続きが次のジョブに渡ることを固定する。
func TestEncodeReconcileWorker_ContinuationSurvivesFreshWorker(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "continuation/1.m2ts", []string{"h265"}, []byte("x"))
	second := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "continuation/2.m2ts", []string{"h265"}, []byte("x"))
	if first >= second {
		t.Fatalf("expected ascending recording IDs, got first=%d second=%d", first, second)
	}
	testutil.MustExec(t, pool, "seeding a retryable failed encode attempt", `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'h265', 'failed', now(), 1)`, first)

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	profiles := encodeConfigWith("h265")
	firstWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, firstWorker, jobs.EncodeReconcileArgs{}), "first reconcile page")
	if got := countEncodeJobs(t, enqueuer, first, "h265"); got != 1 {
		t.Fatalf("first recording jobs after first page = %d, want 1", got)
	}
	if got := countEncodeJobs(t, enqueuer, second, "h265"); got != 0 {
		t.Fatalf("second recording jobs after first page = %d, want 0", got)
	}
	continuations := recordedArgs[jobs.EncodeReconcileArgs](t, enqueuer.CallsOfKind("encode_reconcile"))
	if len(continuations) != 1 {
		t.Fatalf("continuation jobs after first page = %d, want 1", len(continuations))
	}
	if continuations[0].AfterRecordingID != first {
		t.Fatalf("first continuation cursor = %d, want first recording ID %d", continuations[0].AfterRecordingID, first)
	}
	wire, err := json.Marshal(continuations[0])
	testutil.RequireNoError(t, err, "marshaling continuation args")
	var nextPage jobs.EncodeReconcileArgs
	testutil.RequireNoError(t, json.Unmarshal(wire, &nextPage), "unmarshaling continuation args")
	if nextPage.AfterRecordingID != first {
		t.Fatalf("continuation cursor after JSON round trip = %d, want %d", nextPage.AfterRecordingID, first)
	}

	// --once は各ジョブの後にプロセスを終了させる。次ページは新しい worker が引き継ぐ。
	secondWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, secondWorker, nextPage), "second reconcile page")
	if got := countEncodeJobs(t, enqueuer, second, "h265"); got != 1 {
		t.Fatalf("second recording jobs after continuation = %d, want 1", got)
	}
	continuations = recordedArgs[jobs.EncodeReconcileArgs](t, enqueuer.CallsOfKind("encode_reconcile"))
	if len(continuations) != 2 || continuations[1].AfterRecordingID != second {
		t.Fatalf("continuations after second page = %+v, want cursors [%d %d]", continuations, first, second)
	}

	// 末尾を越えたページはもう投入せず、継続の連鎖を止める。
	lastWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, lastWorker, continuations[1]), "page after the last candidate")
	if got := len(recordedArgs[jobs.EncodeReconcileArgs](t, enqueuer.CallsOfKind("encode_reconcile"))); got != 2 {
		t.Errorf("continuation jobs after the last page = %d, want 2", got)
	}
}

func TestEncodeReconcileWorker_ShortPageDoesNotContinue(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "short-page/1.m2ts", []string{"h265"}, []byte("x"))
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), RowLimit: 2, Media: pgstoretest.Media(pool)}
	enqueuer := runEncodeReconcilePass(t, w)
	if got := len(recordedArgs[jobs.EncodeReconcileArgs](t, enqueuer.CallsOfKind("encode_reconcile"))); got != 0 {
		t.Errorf("continuation jobs for a short page = %d, want 0", got)
	}
	if got := countEncodeJobs(t, enqueuer, id, "h265"); got != 1 {
		t.Errorf("encode jobs for the only candidate = %d, want 1", got)
	}
}

func TestEncodeReconcileWorker_RejectsTargetAndContinuationTogether(t *testing.T) {
	t.Parallel()
	w := &EncodeReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer()}
	err := w.Work(context.Background(), jobs.EncodeReconcileArgs{RecordingID: 10, AfterRecordingID: 20})
	if !errors.Is(err, jobq.ErrDiscard) {
		t.Fatalf("Work error = %v, want jobq.ErrDiscard for mixed target and continuation arguments", err)
	}
}

func TestEncodeReconcileWorker_ExhaustedAttemptReturnsAfterRetryRequest(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "retry-exhausted/1.m2ts", []string{"h265"}, []byte("x"))
	testutil.MustExec(t, pool, "seeding an exhausted encode attempt", `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'h265', 'failed', now(), $2)`, id, jobs.EncodeAttemptLimit)
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	profiles := encodeConfigWith("h265")
	firstWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, firstWorker, jobs.EncodeReconcileArgs{}), "reconcile with an exhausted attempt")
	if got := countEncodeJobs(t, enqueuer, id, "h265"); got != 0 {
		t.Fatalf("encode jobs for exhausted candidate = %d, want 0", got)
	}

	deleted, err := sqlcgen.New(pool).DeleteFailedRecordingEncodeAttempts(ctx, sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: id,
		Profiles:    []string{"h265"},
	})
	testutil.RequireNoError(t, err, "resetting failed encode attempts")
	if deleted != 1 {
		t.Fatalf("failed encode attempts deleted = %d, want 1", deleted)
	}
	enqueuer.Reset()
	secondWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), Profiles: profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, secondWorker, jobs.EncodeReconcileArgs{}), "reconcile after retry request")
	if got := countEncodeJobs(t, enqueuer, id, "h265"); got != 1 {
		t.Errorf("encode jobs after retry request = %d, want 1", got)
	}
}

// TestEncodeReconcileWorker_RowLimitCountsRecordings は、1 録画の複数 profile が
// RowLimit を消費せず、継続カーソルが recording_id 単位で進むことを固定する。
func TestEncodeReconcileWorker_RowLimitCountsRecordings(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "row-limit/1.m2ts", []string{"h264", "h265"}, []byte("x"))
	second := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "row-limit/2.m2ts", []string{"h265"}, []byte("x"))
	if first >= second {
		t.Fatalf("expected ascending recording ids, got first=%d second=%d", first, second)
	}

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h264", "h265"), RowLimit: 1, Media: pgstoretest.Media(pool)}
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), first, "h264"); got != 1 {
		t.Errorf("pass 1: encode jobs for first/h264 = %d, want 1", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), first, "h265"); got != 1 {
		t.Errorf("pass 1: encode jobs for first/h265 = %d, want 1 (all profiles of one recording must be returned)", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), second, "h265"); got != 0 {
		t.Errorf("pass 1: encode jobs for second/h265 = %d, want 0 (RowLimit is in recordings)", got)
	}

	continuations := recordedArgs[jobs.EncodeReconcileArgs](t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer).CallsOfKind("encode_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterRecordingID != first {
		t.Fatalf("continuations after first recording = %+v, want cursor %d", continuations, first)
	}
	// 1 profile ではなく 1 録画が RowLimit を消費する。次ページを新しい worker で処理する。
	secondWorker := &EncodeReconcileWorker{Enqueuer: w.Enqueuer, Media: pgstoretest.Media(pool), Profiles: w.Profiles, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), secondWorker, continuations[0]), "second reconcile page")
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), second, "h265"); got != 1 {
		t.Errorf("pass 2: encode jobs for second/h265 = %d, want 1 (cursor must be a recording ID)", got)
	}
}

// TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent は、
// encode.profiles が空の構成でパスが**空振りしていることが見える**ことを固定する。
//
// このパスは known_profiles を SQL に渡すが、Postgres の
// `x = ANY(NULL::text[])` は false ではなく NULL になるため、**nil を渡すと候補も
// 検出も同時に落ちてバックストップが無症状で死ぬ**。config.EncodeConfig.ProfileNames()
// は空設定でも non-nil を返す契約（TestEncodeConfig_ProfileNames_EmptyIsNonNil が
// 固定）なので、ここでは「空 = 候補ゼロだが unsatisfiable に出る」を確認する
// --- ProfileNames が nil を返すようになるとこのテストが落ちる。
func TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "empty/a.m2ts",
		[]string{"h265"}, []byte("x"))

	// プロファイルが 1 つも無い設定。
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{}, Media: pgstoretest.Media(pool)}
	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Errorf("encode jobs = %d, want 0 (nothing can be encoded without profiles)", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 0 {
		t.Errorf("candidates gauge = %v, want 0", got)
	}
	// 空振りが見えること。ここが 0 だと「候補ゼロ」と「クエリが NULL で全部
	// 落ちた」を区別できない。
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("h265")); got != 1 {
		t.Errorf("unsatisfiable gauge for %q = %v, want 1 (an empty profile set must be visible, not silent)", "h265", got)
	}
}

// Enqueuer が無いときはエラーを返す（黙って no-op にすると取りこぼしの回復そのものが消える）。
func TestEncodeReconcileWorker_Work_WithoutEnqueuer_Errors(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	w := &EncodeReconcileWorker{Media: pgstoretest.Media(pool)}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{}); err == nil {
		t.Fatal("expected error when worker has no enqueuer, got nil")
	}
}

func TestEncodeReconcileWorker_Work_WithoutOriginalLocator_Errors(t *testing.T) {
	t.Parallel()
	w := &EncodeReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer()}
	err := jobqfaketest.RunWorkerOnce(t.Context(), w, jobs.EncodeReconcileArgs{})
	if err == nil || err.Error() != "encode reconcile: original locator is nil" {
		t.Fatalf("Work error = %v, want encode reconcile: original locator is nil", err)
	}
}

// ジョブ種別名とキュー・一意化の形。期待値はリテラルで書く（実装の定数と
// 比べると定数を変えたときに何も主張しなくなる）。
func TestEncodeReconcileArgs_KindAndSpec(t *testing.T) {
	t.Parallel()
	if kind := (jobs.EncodeReconcileArgs{}).Kind(); kind != "encode_reconcile" {
		t.Errorf("Kind() = %q, want encode_reconcile", kind)
	}
	spec := jobs.EncodeReconcileArgs{}.Spec()
	if spec.Queue != "cleanup" {
		t.Errorf("Spec().Queue = %q, want cleanup (DB-only reconcile is not store-qualified)", spec.Queue)
	}
	if !spec.Coalesce {
		t.Error("Spec().Coalesce should be true")
	}
}

// TestEncodeReconcile_RebuildRequestOnTrashedRecordingIsNotQueued は、ごみ箱の録画の作り直し要求から
// encode を投入せず、戻った後のパスで投入することを固定する（配線と状態遷移）。どの条件で
// encodable_recordings の外に出るかは SQL 側の TestEncodableRecordings が条件ごとに検査する。
// encodable_recordings の外にある間のパスでは要求行を消さない（消すのは api だけ）。
func TestEncodeReconcile_RebuildRequestOnTrashedRecordingIsNotQueued(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := t.Context()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "review/gate.m2ts", []string{"cut"}, []byte("original"))
	dbtest.AdoptChapters(t, pool, id)
	dbtest.ChapterSpan(t, pool, id)
	keep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, id)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	assetID := seedEncodedAsset(t, pool, id, "cut", "review/gate_cut.mp4")
	testutil.RequireNoError(t, setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}), "setFrozenCuts")
	seedEncodedAssetRebuildRequest(t, pool, id, keep)

	_, err = sqlcgen.New(pool).SoftDeleteRecording(ctx, id)
	testutil.RequireNoError(t, err, "SoftDeleteRecording")
	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}}, Media: pgstoretest.Media(pool)}
	enq := runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, enq, id, "cut"); got != 0 {
		t.Fatalf("encode jobs while the recording is not encodable = %d, want 0", got)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("request rows while not encodable = %d, err %v, want 1", rows, err)
	}

	_, err = sqlcgen.New(pool).RestoreRecording(ctx, id)
	testutil.RequireNoError(t, err, "RestoreRecording")
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, enq, id, "cut"); got != 1 {
		t.Fatalf("encode jobs after the recording is encodable again = %d, want 1", got)
	}
}

func countEncodeJobs(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, recordingID int64, profile string) int {
	t.Helper()
	count := 0
	for _, args := range recordedArgs[jobs.EncodeJobArgs](t, enqueuer.CallsOfKind(jobs.EncodeJobArgs{}.Kind())) {
		if args.RecordingID == recordingID && args.Profile == profile {
			count++
		}
	}
	return count
}
