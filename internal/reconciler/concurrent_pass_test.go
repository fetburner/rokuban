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

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 並走する 2 つの RunPass の割り込みを再現する（docs/data/jobs.md §重複抑止と並走の限界）。
// パス A の mirakc.Client は、指定したリクエストを fake に送る直前に割り込み（パス B の
// 1 パス全体や、mirakc 側の状態遷移）を 1 回だけ実行する。パス A はその時点で既に
// ListSchedules と desired の読みを終えている。DELETE の直前に schedule を観測し直す（GET）ので、
// GET より前の割り込みは A の判定に効き、GET より後（GET の応答から DELETE の到着まで）の割り込みは
// 効かない。後者が残る窓である。

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
			return req.URL.Path == scheduleItemPath(programID)
		}
		return req.URL.Path == "/api/recording/schedules"
	}
}

// scheduleGetRequest は DELETE の直前の観測し直し（GET /api/recording/schedules/{id}）に一致する。
func scheduleGetRequest(programID int64) func(*http.Request) bool {
	return func(req *http.Request) bool {
		return req.Method == http.MethodGet && req.URL.Path == scheduleItemPath(programID)
	}
}

func scheduleItemPath(programID int64) string {
	return fmt.Sprintf("/api/recording/schedules/%d", programID)
}

// slowClient は match に一致するリクエストを、ctx が切れるまで（または slow 経つまで）止める。
// 締切より長く遅らせて締切が効くことを見る。ctx が先に切れたら fake には届かない。
func slowClient(fake *mirakctest.Fake, match func(*http.Request) bool) *mirakc.Client {
	const slow = 2 * time.Second
	return mirakc.NewClient(fake.URL(), &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if match(req) {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(slow):
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})})
}

// removeSchedule は fake から program の schedule を取り除く（mirakc 側で消えたのを再現する）。
func removeSchedule(fake *mirakctest.Fake, programID int64) {
	var rest []mirakc.Schedule
	for id, s := range fake.Schedules() {
		if id != programID {
			rest = append(rest, s)
		}
	}
	fake.SetSchedules(rest)
}

// withheldCount は rokuban_reconcile_write_withheld_total{action,reason} の現在値を返す。
// 他のテストと共有のグローバルなので、呼び出し側は前後の差分で見る。
func withheldCount(action, reason string) float64 {
	return promtestutil.ToFloat64(metrics.ReconcileWriteWithheld.WithLabelValues(action, reason))
}

// concurrentFixture は desired 1 件と、既定の priority で作成済みの schedule を用意する。
type concurrentFixture struct {
	ctx  context.Context
	pool *pgxpool.Pool
	fake *mirakctest.Fake
	res  testReservation
}

func newConcurrentFixture(t *testing.T, programID int64) concurrentFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	fake := mirakctest.New(t)
	res := createReservation(t, pool, programID, "並走", time.Now().Add(time.Hour))
	if err := newReconciler(mirakc.NewClient(fake.URL(), nil), pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass (setup): %v", err)
	}
	if got := fake.Schedules()[programID].Options.Priority; got != 10 {
		t.Fatalf("setup priority = %d, want 10 (DefaultPriority)", got)
	}
	return concurrentFixture{ctx: ctx, pool: pool, fake: fake, res: res}
}

// newStaleDeleteFixture は desired から外れた schedule（skip intent）を持つ fixture を作る。
// 別の予約 1 件を残すのは、desired が空だと全損ブレーカーが削除を止めるため
// （TestReconciler_StaleDeleteAfterNewerObservation_... と同じ作り方）。
func newStaleDeleteFixture(t *testing.T, programID, otherProgramID int64) concurrentFixture {
	t.Helper()
	f := newConcurrentFixture(t, programID)
	createReservation(t, f.pool, otherProgramID, "残る予約", time.Now().Add(time.Hour))
	dbtest.ProgramIntent(t, f.pool, programID, func(p *dbtest.ProgramIntentParams) { p.Action = "skip" })
	return f
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

// 同じ desired で両方が再作成する。B は A の観測の後・A の DELETE の前に再作成を終える。A の
// 観測し直し（GET）は B の結果（差分の無い schedule）を見るので、A は DELETE も POST も送らず見送る。
// DELETE / POST は B の 1 組だけになる。ここで見送りを数えるのは観測し直しが並走を実際に拾う
// 経路だから（ReconcileWriteWithheld の converged）。
func TestReconciler_ConcurrentRecreate_ReobservedAfterPeerRecreateIsWithheld(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017641)
	setPriorityOverride(t, f.pool, f.res, 77)
	lostBefore := promtestutil.ToFloat64(metrics.ReconcileScheduleLost)
	withheldBefore := withheldCount("recreate", "converged")

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		f.pass(t, "B", nil)
	})
	f.pass(t, "A", passA)

	if deletes, posts := f.callsSinceSetup(); deletes != 1 || posts != 1 {
		t.Errorf("DELETE / POST = %d / %d, want 1 / 1 (B の 1 組だけ。A は観測し直しで差分が消えたのを見て見送る)", deletes, posts)
	}
	if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 77 {
		t.Errorf("priority = %d, want 77", got)
	}
	if got := promtestutil.ToFloat64(metrics.ReconcileScheduleLost); got != lostBefore {
		t.Errorf("ReconcileScheduleLost = %v, want %v (見送りは POST 失敗ではない)", got, lostBefore)
	}
	if got := withheldCount("recreate", "converged") - withheldBefore; got != 1 {
		t.Errorf("withheld recreate/converged += %v, want 1", got)
	}
}

// 同じ desired で両方が再作成し、B が A の DELETE と POST の間に割り込む。B は schedule が無い
// のを観測して作成し、A の POST は mirakc に AlreadyExists（400）で拒まれる。schedule は B が
// 作ったもので desired と一致する（収束する）が、A は「DELETE 成功 → POST 失敗」として
// ReconcileScheduleLost を進めるので、このカウンタは並走で偽陽性になる。観測し直しは DELETE の
// 前にしか効かないので、この窓は閉じない。
func TestReconciler_ConcurrentRecreate_CreateBetweenDeleteAndPostCountsFalseLoss(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017642)
	setPriorityOverride(t, f.pool, f.res, 77)
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
			setPriorityOverride(t, f.pool, f.res, 77)

			passA := interleavedClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID), func() {
				setPriorityOverride(t, f.pool, f.res, 99)
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
	setPriorityOverride(t, f.pool, f.res, 77)

	passA := interleavedClient(f.fake, scheduleRequest(http.MethodPost, f.res.ProgramID), func() {
		setPriorityOverride(t, f.pool, f.res, 99)
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
// 観測し直しは desired を読み直さない（所有と state だけを確かめる）ので、この帰結は変わらない。
func TestReconciler_StaleDeleteAfterNewerObservation_LeavesDesiredUnscheduledUntilNextPass(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017645)
	// desired が空だと全損ブレーカーが削除を止め、割り込む DELETE 自体が飛ばない。
	createReservation(t, f.pool, 100000500017647, "残る予約", time.Now().Add(time.Hour))
	dbtest.ProgramIntent(t, f.pool, f.res.ProgramID, func(p *dbtest.ProgramIntentParams) { p.Action = "skip" })

	passA := interleavedClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID), func() {
		if err := pgstoretest.Reservations(f.pool).DeleteIntent(f.ctx, "default", f.res.ProgramID); err != nil {
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

// 残る窓: 観測し直し（GET）は scheduled を見て DELETE に進み、その DELETE の到着の直前に schedule が
// 録画へ遷移する。実物の mirakc はこの DELETE で録画を止め、続く POST は別の record として途中から
// 録り直す（conformance の TestDeleteRecordingScheduleStopsRecorder）。観測し直しは窓を縮めるが、
// GET の応答から DELETE の到着までは閉じられない（mirakc に条件付き DELETE が無い）。
// 録画を落とす。
func TestReconciler_RecreateStillDeletesScheduleThatStartsRecordingInResidualWindow(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017646)
	setPriorityOverride(t, f.pool, f.res, 77)

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
		t.Errorf("DELETE / POST = %d / %d, want 1 / 1 (観測し直しの後に録画へ遷移した schedule に DELETE が届く残る窓)", deletes, posts)
	}
}

// 観測し直し（GET）の前に schedule が録画へ遷移する。GET が recording を見るので、再作成は
// DELETE も POST もせず見送る。録画は止まらず、次のパスは録画中の schedule を触らない。
func TestReconciler_RecreateWithheldWhenScheduleStartsRecordingBeforeReobservation(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017650)
	setPriorityOverride(t, f.pool, f.res, 77)
	withheldBefore := withheldCount("recreate", "state")

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		f.fake.SetScheduleState(f.res.ProgramID, mirakc.ScheduleStateRecording)
	})
	f.pass(t, "A", passA)

	if deletes, posts := f.callsSinceSetup(); deletes != 0 || posts != 0 {
		t.Errorf("DELETE / POST = %d / %d, want 0 / 0 (録画中の schedule は再作成しない)", deletes, posts)
	}
	if got := f.fake.Schedules()[f.res.ProgramID].State; got != mirakc.ScheduleStateRecording {
		t.Errorf("state = %q, want recording (schedule を残す)", got)
	}
	if got := withheldCount("recreate", "state") - withheldBefore; got != 1 {
		t.Errorf("withheld recreate/state += %v, want 1", got)
	}
}

// 削除の経路でも同じ。desired から外れた schedule が観測し直しで録画中になっていたら DELETE しない。
func TestReconciler_DeleteWithheldWhenScheduleStartsRecordingBeforeReobservation(t *testing.T) {
	f := newStaleDeleteFixture(t, 100000500017651, 100000500017652)
	withheldBefore := withheldCount("delete", "state")

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		f.fake.SetScheduleState(f.res.ProgramID, mirakc.ScheduleStateRecording)
	})
	f.pass(t, "A", passA)

	if deletes, _ := f.callsSinceSetup(); deletes != 0 {
		t.Errorf("DELETE = %d, want 0 (録画中の schedule は削除しない)", deletes)
	}
	if got := f.fake.Schedules()[f.res.ProgramID].State; got != mirakc.ScheduleStateRecording {
		t.Errorf("state = %q, want recording (schedule を残す)", got)
	}
	if got := withheldCount("delete", "state") - withheldBefore; got != 1 {
		t.Errorf("withheld delete/state += %v, want 1", got)
	}
}

// 観測し直しで schedule がもう自分のものでなくなっていたら（タグが外れた）削除しない。
func TestReconciler_DeleteWithheldWhenReobservedScheduleIsNotOurs(t *testing.T) {
	f := newStaleDeleteFixture(t, 100000500017653, 100000500017654)
	withheldBefore := withheldCount("delete", "converged")

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		s := f.fake.Schedules()[f.res.ProgramID]
		s.Tags = nil
		f.fake.SetSchedule(s)
	})
	f.pass(t, "A", passA)

	if deletes, _ := f.callsSinceSetup(); deletes != 0 {
		t.Errorf("DELETE = %d, want 0 (所有でなくなった schedule は削除しない)", deletes)
	}
	if _, ok := f.fake.Schedules()[f.res.ProgramID]; !ok {
		t.Error("schedule deleted; want it kept")
	}
	if got := withheldCount("delete", "converged") - withheldBefore; got != 1 {
		t.Errorf("withheld delete/converged += %v, want 1", got)
	}
}

// 観測し直しが 404 なら、既に無いので削除はエラーにも deleted にも数えない。
func TestReconciler_DeleteWithheldWhenReobservedScheduleIsGone(t *testing.T) {
	f := newStaleDeleteFixture(t, 100000500017655, 100000500017656)
	withheldBefore := withheldCount("delete", "gone")
	deletedBefore := promtestutil.ToFloat64(metrics.ReconcileSchedules.WithLabelValues("deleted"))

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		removeSchedule(f.fake, f.res.ProgramID)
	})
	f.pass(t, "A", passA)

	if deletes, _ := f.callsSinceSetup(); deletes != 0 {
		t.Errorf("DELETE = %d, want 0 (観測し直しで消えていたので DELETE を送らない)", deletes)
	}
	if got := promtestutil.ToFloat64(metrics.ReconcileSchedules.WithLabelValues("deleted")) - deletedBefore; got != 0 {
		t.Errorf("ReconcileSchedules deleted += %v, want 0", got)
	}
	if got := withheldCount("delete", "gone") - withheldBefore; got != 1 {
		t.Errorf("withheld delete/gone += %v, want 1", got)
	}
}

// 観測し直しが 404 なら、再作成は POST しない（別のパスが DELETE と POST の間にいる可能性がある）。
// 次のパスが作成する。
func TestReconciler_RecreateWithheldWhenReobservedScheduleIsGone(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017657)
	setPriorityOverride(t, f.pool, f.res, 77)
	withheldBefore := withheldCount("recreate", "gone")

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		removeSchedule(f.fake, f.res.ProgramID)
	})
	f.pass(t, "A", passA)

	if deletes, posts := f.callsSinceSetup(); deletes != 0 || posts != 0 {
		t.Errorf("DELETE / POST = %d / %d, want 0 / 0 (観測し直しで消えていたら何も送らない)", deletes, posts)
	}
	if got := withheldCount("recreate", "gone") - withheldBefore; got != 1 {
		t.Errorf("withheld recreate/gone += %v, want 1", got)
	}

	f.pass(t, "C", nil)
	if got, ok := f.fake.Schedules()[f.res.ProgramID]; !ok || got.Options.Priority != 77 {
		t.Errorf("after next pass: present=%v priority=%d, want present with priority 77", ok, got.Options.Priority)
	}
}

// 観測し直しが 5xx なら、新しい観測なしに DELETE / POST しない。
func TestReconciler_ReobservationErrorWithholdsWrite(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		f := newStaleDeleteFixture(t, 100000500017658, 100000500017659)
		f.fake.FailNext(http.MethodGet, scheduleItemPath(f.res.ProgramID), 1, http.StatusInternalServerError)
		withheldBefore := withheldCount("delete", "error")

		f.pass(t, "A", nil)

		if deletes, _ := f.callsSinceSetup(); deletes != 0 {
			t.Errorf("DELETE = %d, want 0 (観測し直しが失敗したら削除しない)", deletes)
		}
		if _, ok := f.fake.Schedules()[f.res.ProgramID]; !ok {
			t.Error("schedule deleted; want it kept")
		}
		if got := withheldCount("delete", "error") - withheldBefore; got != 1 {
			t.Errorf("withheld delete/error += %v, want 1", got)
		}
	})
	t.Run("recreate", func(t *testing.T) {
		f := newConcurrentFixture(t, 100000500017660)
		setPriorityOverride(t, f.pool, f.res, 77)
		f.fake.FailNext(http.MethodGet, scheduleItemPath(f.res.ProgramID), 1, http.StatusInternalServerError)
		withheldBefore := withheldCount("recreate", "error")

		f.pass(t, "A", nil)

		if deletes, posts := f.callsSinceSetup(); deletes != 0 || posts != 0 {
			t.Errorf("DELETE / POST = %d / %d, want 0 / 0 (観測し直しが失敗したら何も送らない)", deletes, posts)
		}
		if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 10 {
			t.Errorf("priority = %d, want 10 (旧い schedule を残す)", got)
		}
		if got := withheldCount("recreate", "error") - withheldBefore; got != 1 {
			t.Errorf("withheld recreate/error += %v, want 1", got)
		}
	})
}

// 観測し直しの締切（ObserveAndDeleteTimeout）が GET と DELETE を包む。GET が締切より長く
// かかったら、DELETE を送らずに見送る。締切は親の ctx ではない。
func TestReconciler_ReobservationDeadlineWithholdsWrite(t *testing.T) {
	cfg := &reconciler.Config{ObserveAndDeleteTimeout: 50 * time.Millisecond}

	t.Run("delete", func(t *testing.T) {
		f := newStaleDeleteFixture(t, 100000500017661, 100000500017662)
		withheldBefore := withheldCount("delete", "error")
		mc := slowClient(f.fake, scheduleGetRequest(f.res.ProgramID))

		if err := newReconciler(mc, f.pool, cfg).RunPass(f.ctx); err != nil {
			t.Fatalf("RunPass: %v", err)
		}

		if deletes, _ := f.callsSinceSetup(); deletes != 0 {
			t.Errorf("DELETE = %d, want 0 (締切で観測し直しが切れたら削除しない)", deletes)
		}
		if got := withheldCount("delete", "error") - withheldBefore; got != 1 {
			t.Errorf("withheld delete/error += %v, want 1", got)
		}
	})
	t.Run("recreate", func(t *testing.T) {
		f := newConcurrentFixture(t, 100000500017663)
		setPriorityOverride(t, f.pool, f.res, 77)
		withheldBefore := withheldCount("recreate", "error")
		mc := slowClient(f.fake, scheduleGetRequest(f.res.ProgramID))

		if err := newReconciler(mc, f.pool, cfg).RunPass(f.ctx); err != nil {
			t.Fatalf("RunPass: %v", err)
		}

		if deletes, posts := f.callsSinceSetup(); deletes != 0 || posts != 0 {
			t.Errorf("DELETE / POST = %d / %d, want 0 / 0 (締切で観測し直しが切れたら何も送らない)", deletes, posts)
		}
		if got := withheldCount("recreate", "error") - withheldBefore; got != 1 {
			t.Errorf("withheld recreate/error += %v, want 1", got)
		}
	})
}

// 観測し直しの contentPath を引き継ぐ。冒頭の観測と観測し直しの間に mirakc 側の contentPath が
// 変わっていたら、再作成後の schedule は新しい方を持つ。
func TestReconciler_RecreateInheritsContentPathFromReobservation(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017664)
	setPriorityOverride(t, f.pool, f.res, 77)

	passA := interleavedClient(f.fake, scheduleGetRequest(f.res.ProgramID), func() {
		s := f.fake.Schedules()[f.res.ProgramID]
		s.Options.ContentPath = ptrString("reobserved/path.m2ts")
		f.fake.SetSchedule(s)
	})
	f.pass(t, "A", passA)

	var gotPath string
	if p := f.fake.Schedules()[f.res.ProgramID].Options.ContentPath; p != nil {
		gotPath = *p
	}
	if gotPath != "reobserved/path.m2ts" {
		t.Errorf("contentPath = %q, want %q (観測し直した値を引き継ぐ)", gotPath, "reobserved/path.m2ts")
	}
}

// DELETE だけが締切を超える（GET は速い）。締切で切れた DELETE は fake に届かず、schedule は残る。
// 再作成なら POST もしない。締切は DELETE まで包む、という判定を固定する。
func TestReconciler_DeleteBeyondDeadlineIsNotSent(t *testing.T) {
	cfg := &reconciler.Config{ObserveAndDeleteTimeout: 50 * time.Millisecond}

	t.Run("delete", func(t *testing.T) {
		f := newStaleDeleteFixture(t, 100000500017670, 100000500017671)
		mc := slowClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID))

		if err := newReconciler(mc, f.pool, cfg).RunPass(f.ctx); err != nil {
			t.Fatalf("RunPass: %v", err)
		}

		if deletes, _ := f.callsSinceSetup(); deletes != 0 {
			t.Errorf("DELETE reached fake = %d, want 0 (締切で切れた DELETE は届かない)", deletes)
		}
		if _, ok := f.fake.Schedules()[f.res.ProgramID]; !ok {
			t.Error("schedule deleted; want it kept")
		}
	})
	t.Run("recreate", func(t *testing.T) {
		f := newConcurrentFixture(t, 100000500017672)
		setPriorityOverride(t, f.pool, f.res, 77)
		mc := slowClient(f.fake, scheduleRequest(http.MethodDelete, f.res.ProgramID))

		if err := newReconciler(mc, f.pool, cfg).RunPass(f.ctx); err != nil {
			t.Fatalf("RunPass: %v", err)
		}

		if deletes, posts := f.callsSinceSetup(); deletes != 0 || posts != 0 {
			t.Errorf("DELETE / POST = %d / %d, want 0 / 0 (締切で切れた DELETE の後は POST もしない)", deletes, posts)
		}
		if got := f.fake.Schedules()[f.res.ProgramID].Options.Priority; got != 10 {
			t.Errorf("priority = %d, want 10 (旧い schedule を残す)", got)
		}
	})
}

// POST は観測し直しの締切の外で送る。締切付き ctx で POST すると、締切切れで schedule が消える。
// POST を観測用 ctx が終わる（締切の 5s が過ぎる）まで止めてから送らせるので、毎回 5s 待つ。
func TestReconciler_PostIsNotBoundByObservationDeadline(t *testing.T) {
	f := newConcurrentFixture(t, 100000500017673)
	setPriorityOverride(t, f.pool, f.res, 77)
	lostBefore := promtestutil.ToFloat64(metrics.ReconcileScheduleLost)
	cfg := &reconciler.Config{ObserveAndDeleteTimeout: 5 * time.Second}
	var observation context.Context
	mc := mirakc.NewClient(f.fake.URL(), &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if scheduleGetRequest(f.res.ProgramID)(req) {
			observation = req.Context()
		}
		if scheduleRequest(http.MethodPost, f.res.ProgramID)(req) {
			if observation == nil {
				t.Errorf("POST without observation")
				return nil, fmt.Errorf("POST without observation")
			}
			<-observation.Done()
		}
		return http.DefaultTransport.RoundTrip(req)
	})})

	if err := newReconciler(mc, f.pool, cfg).RunPass(f.ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	got, ok := f.fake.Schedules()[f.res.ProgramID]
	if !ok || got.Options.Priority != 77 {
		t.Errorf("after recreate: present=%v priority=%d, want present with priority 77", ok, got.Options.Priority)
	}
	if got := promtestutil.ToFloat64(metrics.ReconcileScheduleLost); got != lostBefore {
		t.Errorf("ReconcileScheduleLost = %v, want %v (締切の外の POST は失敗しない)", got, lostBefore)
	}
}
