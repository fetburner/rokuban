package watcher

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

const testSite = DefaultSite

// record_sweep ジョブが投入され、実際に実行されて未処理の record が処理されること。
// SSE が一切動いていない状態（handleEvent を経由しない）でも、sweep だけで
// finished record を拾って recordings を作り ingest ジョブを投入できることを確認する
// （docs/recording.md §3.3「SSE 断中に finished になった record を sweep が拾う」の
// 受け入れ基準）。
func TestSweepWorker_ProcessesUnsweptRecord(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	const programID int64 = 700000600071234
	networkID, serviceID := int32(32736), int32(1024)
	channelType, channel := "GR", "27"
	q := sqlcgen.New(pool)
	// #27 で番組の事実のスナップショットが program_snapshots に抽出され、
	// reservations への FK が張られたため、予約行より先に program_snapshots を作る。
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        testSite,
		ProgramID:   programID,
		Title:       "record_sweep ワーカーテスト",
		StartAt:     time.Now().Add(-time.Hour),
		DurationMs:  1800000,
		NetworkID:   networkID,
		ServiceID:   serviceID,
		ChannelType: channelType,
		Channel:     channel,
	}); err != nil {
		t.Fatalf("upserting program snapshot fixture: %v", err)
	}
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      testSite,
		ProgramID: programID,
	}); err != nil {
		t.Fatalf("creating reservation fixture: %v", err)
	}

	startAt := mirakc.Milliseconds(time.Now().Add(-time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-time.Hour))
	endTime := mirakc.Milliseconds(time.Now())
	duration := int64(1800000)
	name := "record_sweep テスト番組"

	record := mirakc.Record{
		ID: "record-sweep-untagged-001",
		Program: mirakc.Program{
			ID: programID, EventID: 1, ServiceID: int(serviceID), NetworkID: int(networkID),
			StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name,
		},
		Service:   mirakc.Service{Name: "テスト局", Channel: mirakc.ServiceChannel{Type: channelType, Channel: channel}},
		Tags:      []string{mirakc.ProgramTag(programID)},
		Recording: mirakc.RecordInfo{Status: "finished", StartTime: recStart, EndTime: &endTime},
		Content:   mirakc.ContentInfo{Path: "test.m2ts"},
	}

	fake := newRecordSweepFake(t, []mirakc.Record{record})

	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	w := &SweepWorker{
		MirakcClients:      map[string]*mirakc.Client{testSite: mirakc.NewClient(fake.URL(), nil)},
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
		Enqueuer:           enqueuer,
	}
	if err := jobqtestutil.RunWorkerOnce(ctx, w, jobs.RecordSweepArgs{Site: testSite}); err != nil {
		t.Fatalf("SweepWorker.Work: %v", err)
	}

	var recCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM recordings",
	).Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Fatalf("recordings count = %d, want 1 (record_sweep ジョブが未処理の finished record を拾ったはず)", recCount)
	}

	ingest := enqueuedIngestArgs(enqueuer)
	if len(ingest) != 1 || ingest[0].Site != testSite || ingest[0].RecordID != record.ID {
		t.Errorf("enqueued ingest args = %+v, want one job with site=%q record_id=%q", ingest, testSite, record.ID)
	}

	var markerCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM loop_success_markers WHERE loop = 'record_sweep' AND scope = $1`, testSite,
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying record sweep marker: %v", err)
	}
	if markerCount != 1 {
		t.Errorf("record sweep marker rows = %d, want 1", markerCount)
	}
}

// mirakc の全量取得に失敗した record_sweep は成功扱いにせず、前回成功時刻を
// 進めない。ScaledJob の停止検出で「HTTP 障害なのに新鮮」と誤認させないため。
func TestSweepWorker_DoesNotMarkFailedSweep(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)

	fake := newRecordSweepFake(t, nil)
	fake.FailNext(http.MethodGet, "/api/recording/records", 1, http.StatusServiceUnavailable)

	w := &SweepWorker{
		MirakcClients:      map[string]*mirakc.Client{testSite: mirakc.NewClient(fake.URL(), nil)},
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
	}
	job := jobs.RecordSweepArgs{Site: testSite}
	if err := jobqtestutil.RunWorkerOnce(context.Background(), w, job); err == nil {
		t.Fatal("Work() error = nil, want record list failure")
	}

	var markerCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM loop_success_markers WHERE loop = 'record_sweep' AND scope = $1`, testSite,
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying record sweep marker: %v", err)
	}
	if markerCount != 0 {
		t.Errorf("record sweep marker rows after failed sweep = %d, want 0", markerCount)
	}
}

// newRecordSweepFake は record_sweep が読む mirakc API に空の状態と records を置く。
func newRecordSweepFake(t *testing.T, records []mirakc.Record) *mirakctest.Fake {
	t.Helper()
	fake := mirakctest.New(t)
	fake.SetServices([]mirakc.Service{})
	fake.SetSchedules([]mirakc.Schedule{})
	if records == nil {
		records = []mirakc.Record{}
	}
	fake.SetRecords(records)
	return fake
}

// enqueuedIngestArgs は記録された ingest ジョブの引数だけを返す。
func enqueuedIngestArgs(enqueuer *jobqtestutil.RecordingEnqueuer) []jobs.IngestJobArgs {
	var found []jobs.IngestJobArgs
	for _, call := range enqueuer.CallsOfKind(jobs.IngestJobArgs{}.Kind()) {
		if args, ok := call.Args.(jobs.IngestJobArgs); ok {
			found = append(found, args)
		}
	}
	return found
}

// TestSweepWorker_VerifySiteDirect は jobs.VerifySite（issue #139）を Work() へ
// 直接呼び出して確認する二次防御のテスト。queue 修飾（一次防御。
// internal/jobq/riveradapter/queue_scoping_test.go の TestMultiSiteWorker_OnlyDequeuesOwnSiteQueues）を
// 素通りしてしまうケース（手で INSERT した / 将来のヒント投入のバグで queue と args.Site が
// ずれた場合）への保険が効いていることを、queue 選択を経由せずに固定する。
func TestSweepWorker_VerifySiteDirect(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)

	fake := newRecordSweepFake(t, nil)

	// Enqueuer を置く。nil だと Work は enqueuer の nil チェックで先に落ち、照合を
	// 外しても通ってしまう（照合の判定そのものを測れない）。
	w := &SweepWorker{
		MirakcClients:      map[string]*mirakc.Client{"site-a": mirakc.NewClient(fake.URL(), nil)},
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
		Enqueuer:           jobqtestutil.NewRecordingEnqueuer(),
	}

	job := jobs.RecordSweepArgs{Site: "site-b"}
	err := jobqtestutil.RunWorkerOnce(context.Background(), w, job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", len(calls), err)
	}
}

// TestSweepWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestSweepWorker_VerifySiteDirect と対になる確認）。
func TestSweepWorker_SiteMatch(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	fake := newRecordSweepFake(t, nil)

	w := &SweepWorker{
		MirakcClients:      map[string]*mirakc.Client{"site-a": mirakc.NewClient(fake.URL(), nil)},
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
		Enqueuer:           jobqtestutil.NewRecordingEnqueuer(),
	}
	if err := jobqtestutil.RunWorkerOnce(ctx, w, jobs.RecordSweepArgs{Site: "site-a"}); err != nil {
		t.Fatalf("SweepWorker.Work: %v", err)
	}
	var markerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loop_success_markers WHERE loop = 'record_sweep' AND scope = 'site-a'`).Scan(&markerCount); err != nil {
		t.Fatalf("querying record sweep marker: %v", err)
	}
	if markerCount != 1 {
		t.Errorf("record sweep marker rows = %d, want 1", markerCount)
	}
}
