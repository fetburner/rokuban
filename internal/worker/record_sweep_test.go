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

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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

// RecordSweepSite を指定すると record_sweep が定期ジョブとして投入され、
// 登録済みワーカーが watcher キューで拾うこと（配線の確認）。
func TestRecordSweepPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	srv := newRecordSweepStub(t, nil)
	defer srv.Close()

	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(srv.URL, nil)})
	client, err := NewClient(pool, workers, ClientConfig{
		PeriodicJobs:        true,
		RecordSweepSite:     testSite,
		RecordSweepInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
	})
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

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "record_sweep" {
			t.Errorf("job kind = %q, want %q", event.Job.Kind, "record_sweep")
		}
		if event.Job.Queue != recordSweepQueue {
			t.Errorf("job queue = %q, want %q", event.Job.Queue, recordSweepQueue)
		}
		var args RecordSweepArgs
		if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
			t.Fatalf("unmarshalling job args: %v", err)
		}
		if args.Site != testSite {
			t.Errorf("job args site = %q, want %q", args.Site, testSite)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the periodic record_sweep job")
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

	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(srv.URL, nil)})
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

// TestRecordSweepWorker_SiteMismatch は、job.Args.Site がワーカー自身の site
// （Deps.Site 経由の w.Site）と一致しないジョブが mirakc に一切触れずに
// fail-fast することを確認する（issue #139）。スタブは 200 を返す（弱い
// テストにしないため。issue #139 のテスト規律）。RecordSweepWorker.Work は
// river.ClientFromContextSafely でジョブ実行コンテキストから River クライアント
// を取り出すため、他のワーカーのように w.Work を直接呼べず、実際に
// client.Insert → client.Start でジョブとして実行させる必要がある。
func TestRecordSweepWorker_SiteMismatch(t *testing.T) {
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

	// このプロセスは site-a の mirakc を向いている。
	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(countingSrv.URL, nil), Site: "site-a"})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobFailed)
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

	if _, err := client.Insert(ctx, RecordSweepArgs{Site: "site-b"}, nil); err != nil {
		t.Fatalf("inserting record_sweep job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "record_sweep" {
			t.Fatalf("job kind = %q, want %q", event.Job.Kind, "record_sweep")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for record_sweep job failure")
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc)", got)
	}
}

// TestRecordSweepWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestRecordSweepWorker_SiteMismatch と対になる
// 両方向の確認）。
func TestRecordSweepWorker_SiteMatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	srv := newRecordSweepStub(t, nil)
	defer srv.Close()

	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(srv.URL, nil), Site: "site-a"})
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
