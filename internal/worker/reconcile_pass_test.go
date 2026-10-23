package worker

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

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

	fake := mirakctest.New(t)

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
		MirakcClients:  singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		ScheduleSync:   pgstore.NewScheduleSync(pool),
		Breakers:       pgstore.NewBreakers(pool),
		NeverScheduled: pgstore.NewNeverScheduledEvents(pool),
	}
	if err := runWorkerViaJobQFake(ctx, w, ReconcilePassArgs{Site: testSite}); err != nil {
		t.Fatalf("ReconcilePassWorker.Work: %v", err)
	}

	if n := len(fake.Schedules()); n != 1 {
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
	reservations := pgstore.NewReservations(pool)
	w := &RulerPassWorker{
		RulerPass:      reservations,
		Breakers:       pgstore.NewBreakers(pool),
		NeverScheduled: pgstore.NewNeverScheduledEvents(pool),
		LoopMarkers:    reservations,
		ProgramSearch:  pgstore.NewEPG(pool),
		Enqueuer:       enqueuer,
	}
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

	fake := mirakctest.New(t)

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &ReconcilePassWorker{
		MirakcClients:  singleSiteClients("site-a", mirakc.NewClient(fake.URL(), nil)),
		ScheduleSync:   pgstore.NewScheduleSync(pool),
		Breakers:       pgstore.NewBreakers(pool),
		NeverScheduled: pgstore.NewNeverScheduledEvents(pool),
	}

	job := ReconcilePassArgs{Site: "site-b"}
	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", len(calls), err)
	}
}

// TestReconcilePassWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestReconcilePassWorker_SiteMismatch と対になる
// 両方向の確認）。
func TestReconcilePassWorker_SiteMatch(t *testing.T) {
	pool := testutil.SetupDB(t)

	fake := mirakctest.New(t)

	w := &ReconcilePassWorker{
		MirakcClients:  singleSiteClients("site-a", mirakc.NewClient(fake.URL(), nil)),
		ScheduleSync:   pgstore.NewScheduleSync(pool),
		Breakers:       pgstore.NewBreakers(pool),
		NeverScheduled: pgstore.NewNeverScheduledEvents(pool),
	}

	job := ReconcilePassArgs{Site: "site-a"}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}
}
