package reconciler_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 並走する 2 つの RunPass の割り込みを再現する（docs/data/jobs.md §重複抑止と並走の限界）。
// パス A の mirakc.Client は、指定したリクエストを fake に送る直前に割り込み（パス B の
// 1 パス全体や、mirakc 側の状態遷移）を 1 回だけ実行する。パス A はその時点で既に
// ListSchedules と desired の読みを終えているので、B の書き込みや遷移を知らないまま
// 残りの DELETE / POST を送る。

// interleavedClient は、match に一致する最初のリクエストを送る直前に between を 1 回だけ実行する。
func interleavedClient(fake *mirakctest.Fake, match func(*http.Request) bool, between func()) *mirakc.Client {
	var once sync.Once
	return mirakc.NewClient(fake.URL(), &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if match(req) {
			once.Do(between)
		}
		return http.DefaultTransport.RoundTrip(req)
	})})
}

func scheduleRequest(method string, programID int64) func(*http.Request) bool {
	return func(req *http.Request) bool {
		if req.Method != method {
			return false
		}
		if method == http.MethodDelete {
			return req.URL.Path == fmt.Sprintf("/api/recording/schedules/%d", programID)
		}
		return req.URL.Path == "/api/recording/schedules"
	}
}

// concurrentFixture は desired 1 件と、既定の priority で作成済みの schedule を用意する。
type concurrentFixture struct {
	ctx  context.Context
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
	fake *mirakctest.Fake
	res  sqlcgen.Reservation
}

func newConcurrentFixture(t *testing.T, programID int64) concurrentFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	fake := mirakctest.New(t)
	q := sqlcgen.New(pool)
	res := createReservation(t, ctx, q, programID, "並走", time.Now().Add(time.Hour))
	if err := newReconciler(mirakc.NewClient(fake.URL(), nil), pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass (setup): %v", err)
	}
	if got := fake.Schedules()[programID].Options.Priority; got != 10 {
		t.Fatalf("setup priority = %d, want 10 (DefaultPriority)", got)
	}
	return concurrentFixture{ctx: ctx, pool: pool, q: q, fake: fake, res: res}
}

func (f concurrentFixture) pass(t *testing.T, label string, mc *mirakc.Client) {
	t.Helper()
	if mc == nil {
		mc = mirakc.NewClient(f.fake.URL(), nil)
	}
	if err := newReconciler(mc, f.pool, nil).RunPass(f.ctx); err != nil {
		t.Fatalf("RunPass (%s): %v", label, err)
	}
}

// callsSinceSetup は setup の POST 1 回を除いた、program への DELETE / POST の回数を返す。
func (f concurrentFixture) callsSinceSetup() (deletes, posts int) {
	return countInt64(scheduleCallIDs(f.fake, http.MethodDelete), f.res.ProgramID),
		countInt64(scheduleCallIDs(f.fake, http.MethodPost), f.res.ProgramID) - 1
}

// 同じ desired で両方が再作成し、B が A の DELETE より前に割り込む。A の観測は B の再作成前の
// ものなので、もう差分の無い schedule をもう一度 DELETE → POST する。DELETE → POST が 2 組
// 飛び、schedule の無い窓が 1 回ぶん増えるが、最終状態は desired と一致する（収束する）。
func TestReconciler_ConcurrentRecreate_SecondPairOnAlreadyRecreatedSchedule(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017641)
	setPriorityOverride(t, f.ctx, f.q, f.res, 77)
	lostBefore := promtestutil.ToFloat64(metrics.ReconcileScheduleLost)

	passA := interleavedClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID), func() {
		f.pass(t, "B", nil)
	})
	f.pass(t, "A", passA)

	if deletes, posts := f.callsSinceSetup(); deletes != 2 || posts != 2 {
		t.Errorf("DELETE / POST = %d / %d, want 2 / 2 (A と B がそれぞれ再作成する)", deletes, posts)
	}
	if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 77 {
		t.Errorf("priority = %d, want 77", got)
	}
	if got := promtestutil.ToFloat64(metrics.ReconcileScheduleLost); got != lostBefore {
		t.Errorf("ReconcileScheduleLost = %v, want %v (どちらの POST も成功する)", got, lostBefore)
	}
}

// 同じ desired で両方が再作成し、B が A の DELETE と POST の間に割り込む。B は schedule が無い
// のを観測して作成し、A の POST は mirakc に AlreadyExists（400）で拒まれる。schedule は B が
// 作ったもので desired と一致する（収束する）が、A は「DELETE 成功 → POST 失敗」として
// ReconcileScheduleLost を進めるので、このカウンタは並走で偽陽性になる。
func TestReconciler_ConcurrentRecreate_CreateBetweenDeleteAndPostCountsFalseLoss(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017642)
	setPriorityOverride(t, f.ctx, f.q, f.res, 77)
	lostBefore := promtestutil.ToFloat64(metrics.ReconcileScheduleLost)

	passA := interleavedClient(f.fake, scheduleRequest(http.MethodPost, f.res.ProgramID), func() {
		f.pass(t, "B", nil)
	})
	f.pass(t, "A", passA)

	if deletes, posts := f.callsSinceSetup(); deletes != 1 || posts != 2 {
		t.Errorf("DELETE / POST = %d / %d, want 1 / 2 (A の DELETE、B の作成、A の拒まれた POST)", deletes, posts)
	}
	if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 77 {
		t.Errorf("priority = %d, want 77 (B が作成した schedule)", got)
	}
	if got := promtestutil.ToFloat64(metrics.ReconcileScheduleLost); got != lostBefore+1 {
		t.Errorf("ReconcileScheduleLost = %v, want %v (schedule は在るのに A が lost を数える)", got, lostBefore+1)
	}
}

// 古い desired（priority 77）の A が、新しい desired（99）の B の再作成の後に DELETE → POST を
// 送る。後着の A が勝ち、古い 77 が残る。次のパスが scheduled のうちに走れば 99 に収束する。
// その前に schedule が scheduled を抜けると、再作成のガードが差分を持ち越し続け、録画は古い
// priority のまま走る（チューナーの取り合いで負けて録画を落としうる）。
func TestReconciler_StaleRecreateAfterNewerRecreate_LeavesStalePriority(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stateBeforeC string
		want         int
	}{
		{name: "converges while scheduled", stateBeforeC: mirakc.ScheduleStateScheduled, want: 99},
		{name: "frozen once recording", stateBeforeC: mirakc.ScheduleStateRecording, want: 77},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newConcurrentFixture(t, 100000500017643)
			setPriorityOverride(t, f.ctx, f.q, f.res, 77)

			passA := interleavedClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID), func() {
				setPriorityOverride(t, f.ctx, f.q, f.res, 99)
				f.pass(t, "B", nil)
			})
			f.pass(t, "A", passA)

			if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 77 {
				t.Fatalf("priority after A = %d, want 77 (後着の古い desired が新しい再作成を上書きする)", got)
			}

			f.fake.SetScheduleState(f.res.ProgramID, tc.stateBeforeC)
			f.pass(t, "C", nil)
			if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != tc.want {
				t.Errorf("priority after C = %d, want %d", got, tc.want)
			}
		})
	}
}

// 古い desired（priority 77）の A の DELETE と POST の間に、新しい desired（99）の B が割り込む。
// B は schedule が無いのを観測して 99 で作成し、A の古い POST は AlreadyExists で拒まれる。
// 新しい desired が残る（収束する）。fake が実物と違って POST で上書きしていたら、ここは
// 古い 77 が残る逆の帰結になっていた。
func TestReconciler_StaleRecreatePostAfterNewerCreate_IsRejected(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017644)
	setPriorityOverride(t, f.ctx, f.q, f.res, 77)

	passA := interleavedClient(f.fake, scheduleRequest(http.MethodPost, f.res.ProgramID), func() {
		setPriorityOverride(t, f.ctx, f.q, f.res, 99)
		f.pass(t, "B", nil)
	})
	f.pass(t, "A", passA)

	if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 99 {
		t.Errorf("priority = %d, want 99 (新しい desired の作成が残る)", got)
	}
}

// 古い desired（skip）の A が、新しい desired（skip 解除）の B の観測の後に DELETE を送る。
// B は schedule を観測して差分なしとするので、A の DELETE 後は desired にあるのに schedule が
// 無い。次のパスが作り直す（収束する）が、それまでに番組が始まれば冒頭を失う。上限は
// 「DELETE 成功 → POST 失敗」と同じく reconcile の 1 周期分である。
func TestReconciler_StaleDeleteAfterNewerObservation_LeavesDesiredUnscheduledUntilNextPass(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017645)
	// desired が空だと全損ブレーカーが削除を止め、割り込む DELETE 自体が飛ばない。
	createReservation(t, f.ctx, f.q, 100000500017647, "残る予約", time.Now().Add(time.Hour))
	if _, err := f.q.UpsertProgramIntent(f.ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: f.res.ProgramID, Action: "skip",
	}); err != nil {
		t.Fatalf("recording skip intent: %v", err)
	}

	passA := interleavedClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID), func() {
		if _, err := f.q.DeleteProgramIntent(f.ctx, sqlcgen.DeleteProgramIntentParams{
			Site: "default", ProgramID: f.res.ProgramID,
		}); err != nil {
			t.Fatalf("deleting skip intent: %v", err)
		}
		f.pass(t, "B", nil)
	})
	f.pass(t, "A", passA)

	if _, ok := f.fake.Schedules()[f.res.ProgramID]; ok {
		t.Fatal("schedule exists after A; want it deleted by the stale pass")
	}
	if deletes, posts := f.callsSinceSetup(); deletes != 1 || posts != 0 {
		t.Errorf("DELETE / POST = %d / %d, want 1 / 0 (B は何もしない)", deletes, posts)
	}

	f.pass(t, "C", nil)
	if _, ok := f.fake.Schedules()[f.res.ProgramID]; !ok {
		t.Error("schedule not recreated by the next pass")
	}
}

// 観測で scheduled だった schedule が、パスの DELETE までに録画へ遷移する。state のガードは
// パス冒頭の観測で判定済みなので、再作成の DELETE はそのまま録画中の schedule に届く。
// 実物の mirakc はこの DELETE で録画を止め、続く POST は別の record として途中から録り直す
// （conformance の TestDeleteRecordingScheduleStopsRecorder）。録画を落とす。
//
// 遷移は番組開始の約 15 秒前に起きるので、この窓は単独のパスにもある。並走で効くのは窓の
// 幅である。rescue 後も止まらない旧パスは、観測から ctx の期限（ジョブの Timeout）まで書ける。
func TestReconciler_DeleteReachesScheduleThatStartedRecordingAfterObservation(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017646)
	setPriorityOverride(t, f.ctx, f.q, f.res, 77)

	var stateAtDelete string
	passA := interleavedClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID), func() {
		f.fake.SetScheduleState(f.res.ProgramID, mirakc.ScheduleStateRecording)
		stateAtDelete = f.fake.Schedules()[f.res.ProgramID].State
	})
	f.pass(t, "A", passA)

	if stateAtDelete != mirakc.ScheduleStateRecording {
		t.Fatalf("state at DELETE = %q, want recording (割り込みが DELETE の前に入っていない)", stateAtDelete)
	}
	if deletes, posts := f.callsSinceSetup(); deletes != 1 || posts != 1 {
		t.Errorf("DELETE / POST = %d / %d, want 1 / 1 (録画中の schedule を再作成する)", deletes, posts)
	}
}
