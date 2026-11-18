package reconciler_test

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

const testSite = store.DefaultSite

// reconcile_pass ジョブが投入され、実際に実行されて mirakc に schedule が作られること
// （ロジック自体は internal/reconciler でテスト済みなので、ここでは「ジョブとして
// 呼ばれると本当に動く」という配線だけを確認する）。
func TestPassWorker_CreatesSchedule(t *testing.T) {
	t.Parallel()
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

	w := &reconciler.PassWorker{
		MirakcClients:  map[string]*mirakc.Client{testSite: mirakc.NewClient(fake.URL(), nil)},
		ScheduleSync:   pgstoretest.ScheduleSync(pool),
		Breakers:       pgstoretest.Breakers(pool),
		NeverScheduled: pgstoretest.NeverScheduledEvents(pool),
	}
	if err := jobqtestutil.RunWorkerOnce(ctx, w, jobs.ReconcilePassArgs{Site: testSite}); err != nil {
		t.Fatalf("PassWorker.Work: %v", err)
	}

	if n := len(fake.Schedules()); n != 1 {
		t.Fatalf("mirakc schedule count = %d, want 1 (reconcile_pass ジョブが突き合わせを実行して schedule を作ったはず)", n)
	}
}

// TestPassWorker_SiteMismatch は、job.Args.Site がワーカーの束縛に無い site の
// ジョブが mirakc に一切触れずに fail-fast することを確認する（issue #139）。
// 束縛は site-a だけなので、site-b のジョブは jobs.VerifySite で拒まれる。
// reconciler.RunPass はデスクトップ側の予約が 0 件でも常に GET /api/recording/schedules を
// 行うため、照合が外れていればこの fake に 1 件以上のリクエストが届く。DB は空のままでよい。
func TestPassWorker_SiteMismatch(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)

	fake := mirakctest.New(t)

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &reconciler.PassWorker{
		MirakcClients:  map[string]*mirakc.Client{"site-a": mirakc.NewClient(fake.URL(), nil)},
		ScheduleSync:   pgstoretest.ScheduleSync(pool),
		Breakers:       pgstoretest.Breakers(pool),
		NeverScheduled: pgstoretest.NeverScheduledEvents(pool),
	}

	job := jobs.ReconcilePassArgs{Site: "site-b"}
	err := jobqtestutil.RunWorkerOnce(context.Background(), w, job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", len(calls), err)
	}
}

// TestPassWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestPassWorker_SiteMismatch と対になる
// 両方向の確認）。
func TestPassWorker_SiteMatch(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)

	fake := mirakctest.New(t)

	w := &reconciler.PassWorker{
		MirakcClients:  map[string]*mirakc.Client{"site-a": mirakc.NewClient(fake.URL(), nil)},
		ScheduleSync:   pgstoretest.ScheduleSync(pool),
		Breakers:       pgstoretest.Breakers(pool),
		NeverScheduled: pgstoretest.NeverScheduledEvents(pool),
	}

	job := jobs.ReconcilePassArgs{Site: "site-a"}
	if err := jobqtestutil.RunWorkerOnce(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}
}
