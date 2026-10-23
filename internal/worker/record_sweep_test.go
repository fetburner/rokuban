package worker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// record_sweep は Args.Spec で既定より長い有限の締切を持つ。
func TestRecordSweepWorker_HasGenerousTimeout(t *testing.T) {
	args := RecordSweepArgs{}
	got := args.Spec().Timeout(args)
	if got != 10*time.Minute {
		t.Errorf("Timeout() = %v, want 10m", got)
	}
}

// record_sweep ジョブが投入され、実際に実行されて未処理の record が処理されること。
// SSE が一切動いていない状態（handleEvent を経由しない）でも、sweep だけで
// finished record を拾って recordings を作り ingest ジョブを投入できることを確認する
// （docs/recording.md §3.3「SSE 断中に finished になった record を sweep が拾う」の
// 受け入れ基準）。
func TestRecordSweepWorker_ProcessesUnsweptRecord(t *testing.T) {
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

	enqueuer := newRecordingEnqueuer()
	w := &RecordSweepWorker{
		MirakcClients:      singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
		Enqueuer:           enqueuer,
	}
	if err := runWorkerViaJobQFake(ctx, w, RecordSweepArgs{Site: testSite}); err != nil {
		t.Fatalf("RecordSweepWorker.Work: %v", err)
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

	ingest := enqueuedArgs[jobs.IngestJobArgs](enqueuer, "ingest")
	if len(ingest) != 1 || ingest[0].Site != testSite || ingest[0].RecordID != record.ID {
		t.Errorf("enqueued ingest args = %+v, want one job with site=%q record_id=%q", ingest, testSite, record.ID)
	}

	var markerCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM record_sweep_snapshots WHERE site = $1`, testSite,
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying record sweep marker: %v", err)
	}
	if markerCount != 1 {
		t.Errorf("record sweep marker rows = %d, want 1", markerCount)
	}
}

// mirakc の全量取得に失敗した record_sweep は成功扱いにせず、前回成功時刻を
// 進めない。ScaledJob の停止検出で「HTTP 障害なのに新鮮」と誤認させないため。
func TestRecordSweepWorker_DoesNotMarkFailedSweep(t *testing.T) {
	pool := testutil.SetupDB(t)

	fake := newRecordSweepFake(t, nil)
	fake.FailNext(http.MethodGet, "/api/recording/records", 1, http.StatusServiceUnavailable)

	w := &RecordSweepWorker{
		MirakcClients:      singleSiteClients(testSite, mirakc.NewClient(fake.URL(), nil)),
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
	}
	job := jobs.RecordSweepArgs{Site: testSite}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err == nil {
		t.Fatal("Work() error = nil, want record list failure")
	}

	var markerCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM record_sweep_snapshots WHERE site = $1`, testSite,
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

// TestRecordSweepWorker_VerifySiteDirect は verifySite（issue #139）を Work() へ
// 直接呼び出して確認する二次防御のテスト（epg_test.go の
// TestEpgSyncWorker_SiteMismatch / reconcile_pass_test.go の
// TestReconcilePassWorker_SiteMismatch と同じ形）。queue 修飾（一次防御。
// internal/jobq/riveradapter/queue_scoping_test.go の TestMultiSiteWorker_OnlyDequeuesOwnSiteQueues）を素通りしてしまうケース
// （手で INSERT した / 将来のヒント投入のバグで queue と args.Site がずれた場合）
// への保険が効いていることを、queue 選択を経由せずに固定する。
func TestRecordSweepWorker_VerifySiteDirect(t *testing.T) {
	pool := testutil.SetupDB(t)

	fake := newRecordSweepFake(t, nil)

	w := &RecordSweepWorker{
		MirakcClients:      singleSiteClients("site-a", mirakc.NewClient(fake.URL(), nil)),
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
	}

	job := RecordSweepArgs{Site: "site-b"}
	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", len(calls), err)
	}
}

// TestRecordSweepWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestRecordSweepWorker_VerifySiteDirect と対になる確認）。
func TestRecordSweepWorker_SiteMatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	fake := newRecordSweepFake(t, nil)

	w := &RecordSweepWorker{
		MirakcClients:      singleSiteClients("site-a", mirakc.NewClient(fake.URL(), nil)),
		RecordObservations: pgstoretest.Recordings(pool),
		LoopMarkers:        pgstoretest.Reservations(pool),
		Enqueuer:           newRecordingEnqueuer(),
	}
	if err := runWorkerViaJobQFake(ctx, w, RecordSweepArgs{Site: "site-a"}); err != nil {
		t.Fatalf("RecordSweepWorker.Work: %v", err)
	}
	var markerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_sweep_snapshots WHERE site = 'site-a'`).Scan(&markerCount); err != nil {
		t.Fatalf("querying record sweep marker: %v", err)
	}
	if markerCount != 1 {
		t.Errorf("record sweep marker rows = %d, want 1", markerCount)
	}
}
