package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// record_sweep は無制限にはせず、既定より長い上限を置く（recordSweepTimeout のコメント参照）。
func TestRecordSweepWorker_HasGenerousTimeout(t *testing.T) {
	w := &RecordSweepWorker{}
	got := w.Timeout(nil)
	if got <= river.JobTimeoutDefault {
		t.Errorf("Timeout() = %v, want > JobTimeoutDefault (%v)", got, river.JobTimeoutDefault)
	}
}

// BoundSites を指定すると record_sweep が定期ジョブとして投入され、
// 登録済みワーカーが watcher キューで拾うこと（配線の確認。BoundSites は
// record_sweep 以外の 4 種も同時に登録するが、waitPeriodicJobEvent が
// kind で選り分ける）。
func TestRecordSweepPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)

	srv := newRecordSweepStub(t, nil)
	// t.Cleanup（defer だとクライアント停止より先に走り、動いている最中にスタブを閉じる）。
	t.Cleanup(srv.Close)

	subscribeCh := startPeriodicJobClient(t, pool, &Deps{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil))}, ClientConfig{
		PeriodicJobs:        true,
		BoundSites:          []string{testSite},
		RecordSweepInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
	}, river.EventKindJobCompleted)

	event := waitPeriodicJobEvent(t, subscribeCh, "record_sweep")
	if event.Job.Kind != "record_sweep" {
		t.Errorf("job kind = %q, want %q", event.Job.Kind, "record_sweep")
	}
	wantQueue := jobs.PhysicalQueueName(recordSweepQueue, testSite)
	if event.Job.Queue != wantQueue {
		t.Errorf("job queue = %q, want %q", event.Job.Queue, wantQueue)
	}
	var args RecordSweepArgs
	if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
		t.Fatalf("unmarshalling job args: %v", err)
	}
	if args.Site != testSite {
		t.Errorf("job args site = %q, want %q", args.Site, testSite)
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

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

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

	srv := newRecordSweepStub(t, []mirakc.Record{record})
	defer srv.Close()

	workers := NewWorkers(&Deps{Pool: pool, MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil))})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	if _, err := client.Insert(ctx, RecordSweepArgs{Site: testSite}, nil); err != nil {
		t.Fatalf("inserting record_sweep job: %v", err)
	}

	found := false
	for !found {
		select {
		case event := <-subscribeCh:
			if event.Job.Kind == "record_sweep" {
				found = true
			}
		case <-time.After(20 * time.Second):
			t.Fatal("timed out waiting for record_sweep job completion")
		}
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

	var ingestCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'ingest' AND args->>'record_id' = $1", record.ID,
	).Scan(&ingestCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if ingestCount != 1 {
		t.Errorf("ingest job count = %d, want 1", ingestCount)
	}
}

// UniqueOpts による合流: 同じサイトの record_sweep を 2 回投入すると 1 件しか
// 作られないこと（docs/data.md §2「排他はジョブロック + UniqueOpts」）。
func TestRecordSweep_DuplicateInsertMerges(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	args := RecordSweepArgs{Site: "default"}
	if _, err := client.Insert(ctx, args, nil); err != nil {
		t.Fatalf("inserting first job: %v", err)
	}
	dup, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting duplicate: %v", err)
	}
	if !dup.UniqueSkippedAsDuplicate {
		t.Error("同じサイトの record_sweep を 2 回投入したのに合流しなかった")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'record_sweep'`).Scan(&count); err != nil {
		t.Fatalf("counting river_job rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("river_job count for record_sweep = %d, want 1", count)
	}
}

// newRecordSweepStub は Watcher.Sweep が呼ぶ /api/recording/records と
// /api/services への最小スタブを返す。records が nil なら空リストを返す。
func newRecordSweepStub(t *testing.T, records []mirakc.Record) *httptest.Server {
	t.Helper()
	if records == nil {
		records = []mirakc.Record{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(records)
	})
	mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode([]mirakc.Service{})
	})
	return httptest.NewServer(mux)
}

// TestRecordSweepWorker_SiteMismatch は、job.Args.Site がワーカーの束縛
// サイト集合（Deps.MirakcClients 経由の w.MirakcClients）に含まれないジョブが
// mirakc に一切触れずに fail-fast することを確認する（issue #139）。
// スタブは 200 を返す（弱い
// テストにしないため。issue #139 のテスト規律）。RecordSweepWorker.Work は
// river.ClientFromContextSafely でジョブ実行コンテキストから River クライアント
// を取り出すため、他のワーカーのように w.Work を直接呼べず、実際に
// client.Insert → client.Start でジョブとして実行させる必要がある。
// TestRecordSweepWorker_SiteMismatch_NeverDequeued は queue 修飾（issue #185
// M4-13）による一次防御を確認する: site-a に束縛された worker は watcher_site-a
// しか購読しないので、site-b 向けの record_sweep ジョブ（watcher_site-b に乗る）は
// 一度も掴まれない。verifySite（issue #139）は二次防御であり、ここでは
// キュー選択の時点で分離できていることを、モックへのリクエスト 0 件だけでなく
// 「ジョブが available のまま残る」ことで確認する（issue #185 の受け入れ基準）。
func TestRecordSweepWorker_SiteMismatch_NeverDequeued(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode([]mirakc.Record{})
	})
	mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode([]mirakc.Service{})
	})
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mux.ServeHTTP(w, r)
	}))
	defer countingSrv.Close()

	// このプロセスは site-a に束縛されている: watcher_site-a しか購読しない。
	workers := NewWorkers(&Deps{Pool: pool, MirakcClients: singleSiteClients("site-a", mirakc.NewClient(countingSrv.URL, nil))})
	client, err := NewClient(pool, workers, ClientConfig{BoundSites: []string{"site-a"}})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	res, err := client.Insert(ctx, RecordSweepArgs{Site: "site-b"}, nil)
	if err != nil {
		t.Fatalf("inserting record_sweep job: %v", err)
	}
	wantQueue := jobs.PhysicalQueueName(recordSweepQueue, "site-b")
	if res.Job.Queue != wantQueue {
		t.Fatalf("job queue = %q, want %q", res.Job.Queue, wantQueue)
	}

	// この worker は watcher_site-b を購読していないので、しばらく待っても
	// dequeue されないはず。掴まれてしまえば mirakc への要求が発生するので、
	// 要求が来ないことも合わせて確認する。
	time.Sleep(2 * time.Second)

	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", res.Job.ID).Scan(&state); err != nil {
		t.Fatalf("querying job state: %v", err)
	}
	if state != string(rivertype.JobStateAvailable) {
		t.Errorf("job state = %q, want %q (site-b の worker がいないので dequeue されないはず)",
			state, rivertype.JobStateAvailable)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (job must never be dequeued by the site-a worker)", got)
	}
}

// TestRecordSweepWorker_VerifySiteDirect は verifySite（issue #139）を Work() へ
// 直接呼び出して確認する二次防御のテスト（epg_test.go の
// TestEpgSyncWorker_SiteMismatch / reconcile_pass_test.go の
// TestReconcilePassWorker_SiteMismatch と同じ形）。queue 修飾（一次防御。
// TestRecordSweepWorker_SiteMismatch_NeverDequeued）を素通りしてしまうケース
// （手で INSERT した / 将来のヒント投入のバグで queue と args.Site がずれた場合）
// への保険が効いていることを、queue 選択を経由せずに固定する。
func TestRecordSweepWorker_VerifySiteDirect(t *testing.T) {
	pool := testutil.SetupDB(t)

	var requests atomic.Int32
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer countingSrv.Close()

	w := &RecordSweepWorker{
		MirakcClients: singleSiteClients("site-a", mirakc.NewClient(countingSrv.URL, nil)),
		Pool:          pool,
	}

	job := &river.Job[RecordSweepArgs]{JobRow: &rivertype.JobRow{}, Args: RecordSweepArgs{Site: "site-b"}}
	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", got, err)
	}
}

// TestRecordSweepWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestRecordSweepWorker_SiteMismatch_NeverDequeued /
// TestRecordSweepWorker_VerifySiteDirect と対になる両方向の確認）。
func TestRecordSweepWorker_SiteMatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	srv := newRecordSweepStub(t, nil)
	defer srv.Close()

	workers := NewWorkers(&Deps{Pool: pool, MirakcClients: singleSiteClients("site-a", mirakc.NewClient(srv.URL, nil))})
	client, err := NewClient(pool, workers, ClientConfig{BoundSites: []string{"site-a"}})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	if _, err := client.Insert(ctx, RecordSweepArgs{Site: "site-a"}, nil); err != nil {
		t.Fatalf("inserting record_sweep job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "record_sweep" {
			t.Fatalf("job kind = %q, want %q", event.Job.Kind, "record_sweep")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for record_sweep job completion")
	}
}
