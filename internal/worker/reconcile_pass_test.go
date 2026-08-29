package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// scheduleStub は reconcile_pass のワーカーテスト専用の最小 mirakc スタブ。
// internal/reconciler のテストは同等のロジック（GET/POST/DELETE schedules）を
// 別パッケージ（reconciler_test）に持っているため共有できず、ここでは
// 「ジョブとして呼ばれると本当に mirakc へ届く」という配線だけを見るための
// 最小構成にする。
type scheduleStub struct {
	mu        sync.Mutex
	schedules map[int64]mirakc.Schedule
}

func newScheduleStub() *scheduleStub {
	return &scheduleStub{schedules: make(map[int64]mirakc.Schedule)}
}

func (s *scheduleStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/recording/schedules":
		list := make([]mirakc.Schedule, 0, len(s.schedules))
		for _, sc := range s.schedules {
			list = append(list, sc)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)

	case r.Method == http.MethodPost && r.URL.Path == "/api/recording/schedules":
		var input mirakc.ScheduleInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sc := mirakc.Schedule{
			State:   "scheduled",
			Program: mirakc.Program{ID: input.ProgramID},
			Options: input.Options,
			Tags:    input.Tags,
		}
		s.schedules[input.ProgramID] = sc
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sc)

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *scheduleStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.schedules)
}

// reconciler は無制限にはせず、既定より長い上限を置く（reconcilePassTimeout の
// コメント参照）。
func TestReconcilePassWorker_HasGenerousTimeout(t *testing.T) {
	w := &ReconcilePassWorker{}
	got := w.Timeout(nil)
	if got <= river.JobTimeoutDefault {
		t.Errorf("Timeout() = %v, want > JobTimeoutDefault (%v)", got, river.JobTimeoutDefault)
	}
}

// ReconcilePassSite を指定すると reconcile_pass が定期ジョブとして投入され、
// 登録済みワーカーが reconciler キューで拾うこと（配線の確認）。
func TestReconcilePassPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)

	stub := newScheduleStub()
	srv := httptest.NewServer(stub)
	// t.Cleanup（LIFO で startPeriodicJobClient のクライアント停止より後に走る）。
	// defer だと関数の return 直後に走り、River client がまだ動いている最中に
	// mirakc スタブを閉じてしまう。
	t.Cleanup(srv.Close)

	subscribeCh := startPeriodicJobClient(t, pool, &Deps{MirakcClient: mirakc.NewClient(srv.URL, nil)}, ClientConfig{
		PeriodicJobs:          true,
		ReconcilePassSite:     testSite,
		ReconcilePassInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
	}, river.EventKindJobCompleted)

	event := waitPeriodicJobEvent(t, subscribeCh, "reconcile_pass")
	if event.Job.Kind != "reconcile_pass" {
		t.Errorf("job kind = %q, want %q", event.Job.Kind, "reconcile_pass")
	}
	wantQueue := qualifyQueueName(reconcilerQueue, testSite)
	if event.Job.Queue != wantQueue {
		t.Errorf("job queue = %q, want %q", event.Job.Queue, wantQueue)
	}
	var args ReconcilePassArgs
	if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
		t.Fatalf("unmarshalling job args: %v", err)
	}
	if args.Site != testSite {
		t.Errorf("job args site = %q, want %q", args.Site, testSite)
	}
}

// reconcile_pass ジョブが投入され、実際に実行されて mirakc に schedule が作られること
// （ロジック自体は internal/reconciler でテスト済みなので、ここでは「ジョブとして
// 呼ばれると本当に動く」という配線だけを確認する）。
func TestReconcilePassWorker_CreatesSchedule(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	stub := newScheduleStub()
	srv := httptest.NewServer(stub)
	defer srv.Close()

	networkID, serviceID := int32(40000), int32(6000)
	channelType, channel := "GR", "27"
	const programID int64 = 400000600061234
	q := sqlcgen.New(pool)
	// #27 で番組の事実のスナップショットが program_snapshots に抽出され、
	// reservations への FK が張られたため、予約行より先に program_snapshots を作る。
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        testSite,
		ProgramID:   programID,
		Title:       "reconcile_pass ワーカーテスト",
		StartAt:     time.Now().Add(time.Hour),
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

	if _, err := client.Insert(ctx, ReconcilePassArgs{Site: testSite}, nil); err != nil {
		t.Fatalf("inserting reconcile_pass job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "reconcile_pass" {
			t.Fatalf("job kind = %q, want %q", event.Job.Kind, "reconcile_pass")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for reconcile_pass job completion")
	}

	if n := stub.count(); n != 1 {
		t.Fatalf("mirakc schedule count = %d, want 1 (reconcile_pass ジョブが突き合わせを実行して schedule を作ったはず)", n)
	}
}

// UniqueOpts による合流: 同じサイトの reconcile_pass を 2 回投入すると 1 件しか
// 作られないこと（docs/data.md §2「排他はジョブロック + UniqueOpts」）。
func TestReconcilePass_DuplicateInsertMerges(t *testing.T) {
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

	args := ReconcilePassArgs{Site: testSite}
	if _, err := client.Insert(ctx, args, nil); err != nil {
		t.Fatalf("inserting first job: %v", err)
	}
	dup, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting duplicate: %v", err)
	}
	if !dup.UniqueSkippedAsDuplicate {
		t.Error("同じサイトの reconcile_pass を 2 回投入したのに合流しなかった")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'reconcile_pass'`).Scan(&count); err != nil {
		t.Fatalf("counting river_job rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("river_job count for reconcile_pass = %d, want 1", count)
	}
}

// ruler パスの完了は reconcile_pass 起動契機のヒントの 1 つ（docs/recording.md
// §3.2「ruler パスの完了」）。base が変われば mirakc に反映すべき差分が増えるため、
// reconcile を早める。
func TestRulerPassWorker_EnqueuesReconcilePassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	// ヒントとして投入された reconcile_pass ジョブは同じクライアントが reconciler
	// キューも引くため実行される。ReconcilePassWorker は MirakcClient に依存するので、
	// ruler 単体のテストであってもここでは mirakc スタブを与えておく必要がある
	// （与えないと reconcile_pass の実行が nil pointer で panic する）。
	stub := newScheduleStub()
	srv := httptest.NewServer(stub)
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

	if _, err := client.Insert(ctx, RulerPassArgs{Site: testSite}, nil); err != nil {
		t.Fatalf("inserting ruler_pass job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "ruler_pass" {
			t.Fatalf("job kind = %q, want %q", event.Job.Kind, "ruler_pass")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for ruler_pass job completion")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'reconcile_pass' AND (args->>'site') = $1`, testSite,
	).Scan(&count); err != nil {
		t.Fatalf("counting reconcile_pass jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("reconcile_pass job count after ruler_pass completion = %d, want 1 "+
			"(ruler_pass 完了時にヒントとして投入されるはず)", count)
	}
}

// TestReconcilePassWorker_SiteMismatch は、job.Args.Site がワーカー自身の site
// （w.Site）と一致しないジョブが mirakc に一切触れずに fail-fast することを
// 確認する（issue #139）。scheduleStub は 200 を返す（弱いテストにしないため。
// issue #139 のテスト規律）。reconciler.RunPass はデスクトップ側の予約が
// 0 件でも常に GET /api/recording/schedules を行う（reconciler.go の RunPass
// 実装）ため、DB は空のままでよい。
func TestReconcilePassWorker_SiteMismatch(t *testing.T) {
	pool := testutil.SetupDB(t)

	var requests atomic.Int32
	stub := newScheduleStub()
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		stub.ServeHTTP(w, r)
	}))
	defer countingSrv.Close()

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &ReconcilePassWorker{
		MirakcClient: mirakc.NewClient(countingSrv.URL, nil),
		Pool:         pool,
		Site:         "site-a",
	}

	job := &river.Job[ReconcilePassArgs]{JobRow: &rivertype.JobRow{}, Args: ReconcilePassArgs{Site: "site-b"}}
	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", got, err)
	}
}

// TestReconcilePassWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestReconcilePassWorker_SiteMismatch と対になる
// 両方向の確認）。
func TestReconcilePassWorker_SiteMatch(t *testing.T) {
	pool := testutil.SetupDB(t)

	stub := newScheduleStub()
	srv := httptest.NewServer(stub)
	defer srv.Close()

	w := &ReconcilePassWorker{
		MirakcClient: mirakc.NewClient(srv.URL, nil),
		Pool:         pool,
		Site:         "site-a",
	}

	job := &river.Job[ReconcilePassArgs]{JobRow: &rivertype.JobRow{}, Args: ReconcilePassArgs{Site: "site-a"}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}
}
