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

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
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

// reconciler は Args.Spec で既定より長い有限の締切を持つ。
func TestReconcilePassWorker_HasGenerousTimeout(t *testing.T) {
	args := ReconcilePassArgs{}
	got := args.Spec().Timeout(args)
	if got != 10*time.Minute {
		t.Errorf("Timeout() = %v, want 10m", got)
	}
}

// reconcile_pass ジョブが投入され、実際に実行されて mirakc に schedule が作られること
// （ロジック自体は internal/reconciler でテスト済みなので、ここでは「ジョブとして
// 呼ばれると本当に動く」という配線だけを確認する）。
func TestReconcilePassWorker_CreatesSchedule(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

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

	w := &ReconcilePassWorker{
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		Pool:          pool,
	}
	if err := runWorkerViaJobQFake(ctx, w, ReconcilePassArgs{Site: testSite}); err != nil {
		t.Fatalf("ReconcilePassWorker.Work: %v", err)
	}

	if n := stub.count(); n != 1 {
		t.Fatalf("mirakc schedule count = %d, want 1 (reconcile_pass ジョブが突き合わせを実行して schedule を作ったはず)", n)
	}
}

// ruler パスの完了は reconcile_pass 起動契機のヒントの 1 つ（docs/recording.md
// §3.2「ruler パスの完了」）。base が変われば mirakc に反映すべき差分が増えるため、
// reconcile を早める。
func TestRulerPassWorker_EnqueuesReconcilePassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := newRecordingEnqueuer()
	w := &RulerPassWorker{Pool: pool, Enqueuer: enqueuer}
	if err := runWorkerViaJobQFake(ctx, w, RulerPassArgs{Site: testSite}); err != nil {
		t.Fatalf("RulerPassWorker.Work: %v", err)
	}
	hints := enqueuedArgs[jobs.ReconcilePassArgs](enqueuer, "reconcile_pass")
	if len(hints) != 1 || hints[0].Site != testSite {
		t.Fatalf("enqueued reconcile_pass hints = %+v, want one hint for site %q", hints, testSite)
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
		MirakcClients: singleSiteClients("site-a", mirakc.NewClient(countingSrv.URL, nil)),
		Pool:          pool,
	}

	job := ReconcilePassArgs{Site: "site-b"}
	err := runWorkerViaJobQFake(context.Background(), w, job)
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
		MirakcClients: singleSiteClients("site-a", mirakc.NewClient(srv.URL, nil)),
		Pool:          pool,
	}

	job := ReconcilePassArgs{Site: "site-a"}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}
}
