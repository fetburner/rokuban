package reconciler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/testutil"
)

type mockMirakc struct {
	mu        sync.Mutex
	schedules map[int64]mirakc.Schedule

	// deleteCalls / postCalls は DELETE / POST が呼ばれた順に programID を記録する。
	// 再作成（DELETE→POST）の呼ばれ方をテストで確認するため。
	deleteCalls []int64
	postCalls   []int64

	// failPostOnce に programID が入っていれば、その programID への次の POST を
	// 1 回だけ 500 で失敗させる（DELETE 成功 → POST 失敗のシナリオを注入する）。
	failPostOnce map[int64]bool
}

func newMockMirakc() *mockMirakc {
	return &mockMirakc{
		schedules:    make(map[int64]mirakc.Schedule),
		failPostOnce: make(map[int64]bool),
	}
}

func (m *mockMirakc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case r.Method == "GET" && r.URL.Path == "/api/recording/schedules":
		var list []mirakc.Schedule
		for _, s := range m.schedules {
			list = append(list, s)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)

	case r.Method == "POST" && r.URL.Path == "/api/recording/schedules":
		var input mirakc.ScheduleInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		m.postCalls = append(m.postCalls, input.ProgramID)
		if m.failPostOnce[input.ProgramID] {
			delete(m.failPostOnce, input.ProgramID)
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		_, _, eventID := mirakc.SplitProgramID(input.ProgramID)
		s := mirakc.Schedule{
			State: "scheduled",
			Program: mirakc.Program{
				ID:       input.ProgramID,
				EventID:  eventID,
				Duration: ptrInt64(300000),
			},
			Options: input.Options,
			Tags:    input.Tags,
		}
		m.schedules[input.ProgramID] = s
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(s)

	case r.Method == "DELETE" && len(r.URL.Path) > len("/api/recording/schedules/"):
		var programID int64
		_, _ = fmt.Sscanf(r.URL.Path, "/api/recording/schedules/%d", &programID)
		m.deleteCalls = append(m.deleteCalls, programID)
		delete(m.schedules, programID)
		w.WriteHeader(200)

	default:
		http.Error(w, "not found", 404)
	}
}

func (m *mockMirakc) getSchedules() map[int64]mirakc.Schedule {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[int64]mirakc.Schedule, len(m.schedules))
	for k, v := range m.schedules {
		cp[k] = v
	}
	return cp
}

func (m *mockMirakc) getDeleteCalls() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]int64, len(m.deleteCalls))
	copy(cp, m.deleteCalls)
	return cp
}

func (m *mockMirakc) getPostCalls() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]int64, len(m.postCalls))
	copy(cp, m.postCalls)
	return cp
}

func (m *mockMirakc) setFailPostOnce(programID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failPostOnce[programID] = true
}

func countInt64(xs []int64, v int64) int {
	n := 0
	for _, x := range xs {
		if x == v {
			n++
		}
	}
	return n
}

func ptrInt64(v int64) *int64 { return &v }

// createReservation は networkID=10000/serviceID=5000/GR/27 のチャンネル
// スナップショット（contentpath_test.go と同じ値）を持つ手動予約を作る。
func createReservation(t *testing.T, ctx context.Context, q *sqlcgen.Queries, programID int64, title string, startAt time.Time) sqlcgen.Reservation {
	t.Helper()
	networkID, serviceID := int32(10000), int32(5000)
	channelType, channel := "GR", "27"
	res, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              "default",
		ProgramID:         programID,
		Title:             title,
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
		NetworkID:         &networkID,
		ServiceID:         &serviceID,
		ChannelType:       &channelType,
		Channel:           &channel,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	return res
}

// setPriorityOverride は program_overrides.overrides に priority を書き込み、
// 予約の effective priority を base とは別の値に上書きする
// （db.EffectiveOptions が base と overrides をマージする経路をそのまま使う。
// overrides は M2-4 で program_intents から program_overrides に分離済み）。
func setPriorityOverride(t *testing.T, ctx context.Context, q *sqlcgen.Queries, res sqlcgen.Reservation, priority int) {
	t.Helper()
	overrides, err := json.Marshal(map[string]int{"priority": priority})
	if err != nil {
		t.Fatalf("marshalling overrides: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:              "default",
		ProgramID:         res.ProgramID,
		Overrides:         overrides,
		ProgramStartAt:    res.ProgramStartAt,
		ProgramDurationMs: res.ProgramDurationMs,
	}); err != nil {
		t.Fatalf("setting priority override: %v", err)
	}
}

func TestReconciler_CreatesSchedule(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	// programID=100000500011234 -> networkID=10000, serviceID=5000（contentpath_test.go と同じ値）。
	// reconciler は予約行のスナップショットのみを見るので、テストのフィクスチャも
	// スナップショットさせておく必要がある（もう programId からの算術には頼らない）。
	networkID, serviceID := int32(10000), int32(5000)
	channelType, channel := "GR", "27"
	res, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              "default",
		ProgramID:         100000500011234,
		Title:             "テスト番組",
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
		NetworkID:         &networkID,
		ServiceID:         &serviceID,
		ChannelType:       &channelType,
		Channel:           &channel,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(schedules))
	}

	s, ok := schedules[100000500011234]
	if !ok {
		t.Fatal("schedule not found for programID 100000500011234")
	}

	resID, found := mirakc.FindReservationID(s.Tags)
	if !found {
		t.Fatal("reservation tag not found in schedule tags")
	}
	if resID != res.ID {
		t.Errorf("tag reservation ID = %d, want %d", resID, res.ID)
	}
	if s.Options.ContentPath == nil || *s.Options.ContentPath == "" {
		t.Error("contentPath is empty")
	}
}

func TestReconciler_DeletesOrphanedSchedule(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	mock.schedules[999] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 999},
		Tags:    []string{"rokuban:reservation=9999"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		MaxDeletesPerPass: 10,
	})

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 0 {
		t.Errorf("expected 0 schedules after cleanup, got %d", len(schedules))
	}
}

func TestReconciler_CircuitBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	for i := int64(1); i <= 20; i++ {
		mock.schedules[i] = mirakc.Schedule{
			State:   "scheduled",
			Program: mirakc.Program{ID: i},
			Tags:    []string{fmt.Sprintf("rokuban:reservation=%d", i)},
		}
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		MaxDeletesPerPass: 5,
	})

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 20 {
		t.Errorf("circuit breaker should prevent deletion, got %d schedules (expected 20)", len(schedules))
	}
}

func TestReconciler_IgnoresNonRokubanSchedules(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	mock.schedules[888] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 888},
		Tags:    []string{"epgstation:rule=42"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)

	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 1 {
		t.Errorf("non-rokuban schedule should not be deleted, got %d", len(schedules))
	}
}

func TestReconciler_SkippedReservationNotScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	// 予約行があっても intent{skip} があれば schedule を作らない。
	// （通常の取消は行ごと落とすが、ruler が作り直した直後などに両方が
	// 共存しうるので、reconciler 側でも意図を尊重することを確かめる）
	_, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              "default",
		ProgramID:         200000500011234,
		Title:             "スキップ番組",
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:              "default",
		ProgramID:         200000500011234,
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
	}); err != nil {
		t.Fatalf("recording skip intent: %v", err)
	}

	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 0 {
		t.Errorf("skipped reservation should not create schedule, got %d", len(schedules))
	}
}

// service_id が NULL の予約（移行前の残骸で、番組が EPG プロジェクションから
// 既に消えているケース）は、programId からの算術で推測せず、schedule を作らずに
// スキップすること。
func TestReconciler_NullServiceIDNotScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	// CreateManualReservation に channel snapshot を渡さない = network_id/service_id
	// などが NULL のまま。移行前の行を模す。
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              "default",
		ProgramID:         300000500011234,
		Title:             "移行前の残骸",
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 0 {
		t.Errorf("reservation with NULL service_id should not create a schedule, got %d", len(schedules))
	}
}

// --- 予約オプションの差分反映（issue #19、docs/recording.md §3.2）---

// priority を変えたら DELETE + POST が飛び、新しい priority で作り直されること。
func TestReconciler_RecreatesOnPriorityChange(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019001, "優先度変更", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}

	s, ok := mock.getSchedules()[res.ProgramID]
	if !ok {
		t.Fatal("schedule not created")
	}
	if s.Options.Priority != 10 {
		t.Fatalf("initial priority = %d, want 10 (DefaultPriority)", s.Options.Priority)
	}

	setPriorityOverride(t, ctx, q, res, 77)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}

	if calls := mock.getDeleteCalls(); countInt64(calls, res.ProgramID) != 1 {
		t.Errorf("delete calls for program %d = %v, want exactly 1", res.ProgramID, calls)
	}
	if calls := mock.getPostCalls(); countInt64(calls, res.ProgramID) != 2 {
		t.Errorf("post calls for program %d = %v, want exactly 2 (create + recreate)", res.ProgramID, calls)
	}

	got, ok := mock.getSchedules()[res.ProgramID]
	if !ok {
		t.Fatal("schedule missing after recreate")
	}
	if got.Options.Priority != 77 {
		t.Errorf("priority after recreate = %d, want 77", got.Options.Priority)
	}
	resID, found := mirakc.FindReservationID(got.Tags)
	if !found || resID != res.ID {
		t.Errorf("tag after recreate = %v, want reservation id %d", got.Tags, res.ID)
	}
}

// state=recording の schedule は priority が食い違っていても一切触らない
// （受け入れ条件: 録画中の予約を破壊しないこと）。
func TestReconciler_RecordingStateNotRecreated(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019002, "録画中", startAt)
	setPriorityOverride(t, ctx, q, res, 99)

	mock.schedules[res.ProgramID] = mirakc.Schedule{
		State:   mirakc.ScheduleStateRecording,
		Program: mirakc.Program{ID: res.ProgramID},
		Options: mirakc.Options{Priority: 5},
		Tags:    []string{mirakc.ReservationTag(res.ID)},
	}

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if calls := mock.getDeleteCalls(); len(calls) != 0 {
		t.Errorf("state=recording schedule should not be deleted, got %v", calls)
	}
	if calls := mock.getPostCalls(); len(calls) != 0 {
		t.Errorf("state=recording schedule should not be recreated, got %v", calls)
	}
	if got := mock.getSchedules()[res.ProgramID].Options.Priority; got != 5 {
		t.Errorf("priority for state=recording schedule changed: got %d, want unchanged 5", got)
	}

	// state ガードで見送った差分は pending_diff{action="update"} に数えない。
	// このゲージは「ゼロに戻らないまま続く = 収束できていない」を読むためのもの
	// （metrics.ReconcilePendingDiff のコメント）。録画中の番組の priority 変更は
	// 録画が終わるまで（数時間）差分が残り続ける正常な状態なので、update に混ぜると
	// 正常なユーザー操作でアラートが鳴る。update_deferred に分けて数える。
	if got := gaugeValue(t, "update"); got != 0 {
		t.Errorf("pending_diff{action=update} = %v, want 0 "+
			"(state ガードで見送った分を混ぜるとゲージがアラート不能になる)", got)
	}
	if got := gaugeValue(t, "update_deferred"); got != 1 {
		t.Errorf("pending_diff{action=update_deferred} = %v, want 1", got)
	}
}

// gaugeValue は metrics.ReconcilePendingDiff の指定ラベルの現在値を読む。
// ゲージはプロセス全体で共有だが、RunPass が毎パス全ラベルを Set するので
// RunPass の直後に読めば前のテストの値は残らない。
func gaugeValue(t *testing.T, action string) float64 {
	t.Helper()
	return promtestutil.ToFloat64(metrics.ReconcilePendingDiff.WithLabelValues(action))
}

// state=tracking / rescheduling の schedule も allowlist により触らないこと。
func TestReconciler_TrackingAndReschedulingStatesNotRecreated(t *testing.T) {
	states := []string{mirakc.ScheduleStateTracking, mirakc.ScheduleStateRescheduling}
	for i, state := range states {
		t.Run(state, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()

			mock := newMockMirakc()
			srv := httptest.NewServer(mock)
			defer srv.Close()

			mc := mirakc.NewClient(srv.URL, nil)
			q := sqlcgen.New(pool)

			startAt := time.Now().Add(1 * time.Hour)
			programID := int64(100000500019100 + i)
			res := createReservation(t, ctx, q, programID, "state-guard", startAt)
			setPriorityOverride(t, ctx, q, res, 55)

			mock.schedules[res.ProgramID] = mirakc.Schedule{
				State:   state,
				Program: mirakc.Program{ID: res.ProgramID},
				Options: mirakc.Options{Priority: 5},
				Tags:    []string{mirakc.ReservationTag(res.ID)},
			}

			rec := reconciler.New("default", mc, pool, nil)
			if err := rec.RunPass(ctx); err != nil {
				t.Fatalf("RunPass: %v", err)
			}

			if calls := mock.getDeleteCalls(); len(calls) != 0 {
				t.Errorf("state=%s schedule should not be deleted, got %v", state, calls)
			}
			if calls := mock.getPostCalls(); len(calls) != 0 {
				t.Errorf("state=%s schedule should not be recreated, got %v", state, calls)
			}
		})
	}
}

// priority が一致していれば何もしない（無駄な再作成 = churn が起きない）こと。
func TestReconciler_NoRecreateWhenPriorityMatches(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019004, "churnなし", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}

	if calls := mock.getDeleteCalls(); len(calls) != 0 {
		t.Errorf("unchanged priority should not trigger any DELETE, got %v", calls)
	}
	if calls := mock.getPostCalls(); countInt64(calls, res.ProgramID) != 1 {
		t.Errorf("unchanged priority should not trigger a recreate POST, got post calls %v", calls)
	}
}

// 再作成の POST は observed の contentPath を引き継ぐこと（テンプレートから
// 再生成しない）。
func TestReconciler_RecreateKeepsObservedContentPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019005, "パス引き継ぎ", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}

	// mirakc 側の観測値を、テンプレートから再生成すれば絶対に出てこない番兵値に
	// 差し替える。再作成後にこの値がそのまま（Sanitize 済みで）引き継がれて
	// いれば、再生成していないことの証拠になる。
	sentinel := "custom/observed-sentinel"
	mock.mu.Lock()
	s := mock.schedules[res.ProgramID]
	s.Options.ContentPath = &sentinel
	mock.schedules[res.ProgramID] = s
	mock.mu.Unlock()

	setPriorityOverride(t, ctx, q, res, 42) // 再作成の契機は priority 変更

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}

	got := mock.getSchedules()[res.ProgramID]
	want := contentpath.SanitizeContentPath(sentinel)
	if got.Options.ContentPath == nil || *got.Options.ContentPath != want {
		t.Errorf("recreated contentPath = %v, want %q (observed value carried over, not regenerated)",
			got.Options.ContentPath, want)
	}
}

// tag が食い違っていたら priority が同じでも再作成されること
// （古い tag が残ると ingest が録画を別の予約に紐付けてしまうため）。
func TestReconciler_RecreateOnTagMismatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019006, "tag不一致", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}

	// priority には触れず、tag だけを別の（実在しない）reservation id に差し替える。
	staleTag := mirakc.ReservationTag(res.ID + 999999)
	mock.mu.Lock()
	s := mock.schedules[res.ProgramID]
	s.Tags = []string{staleTag}
	mock.schedules[res.ProgramID] = s
	mock.mu.Unlock()

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}

	if calls := mock.getDeleteCalls(); countInt64(calls, res.ProgramID) != 1 {
		t.Errorf("tag mismatch should trigger exactly 1 delete, got %v", calls)
	}
	got := mock.getSchedules()[res.ProgramID]
	resID, found := mirakc.FindReservationID(got.Tags)
	if !found || resID != res.ID {
		t.Errorf("tag after recreate = %v, want reservation id %d", got.Tags, res.ID)
	}
}

// tag のない外部産 schedule は priority が食い違っていても触らないこと
// （外部が作った schedule と取り合いにならないため）。
func TestReconciler_ExternalScheduleNotRecreated(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019007, "外部産", startAt)
	setPriorityOverride(t, ctx, q, res, 99)

	mock.schedules[res.ProgramID] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: res.ProgramID},
		Options: mirakc.Options{Priority: 5},
		Tags:    []string{"epgstation:rule=1"},
	}

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if calls := mock.getDeleteCalls(); len(calls) != 0 {
		t.Errorf("external schedule should not be deleted, got %v", calls)
	}
	if calls := mock.getPostCalls(); len(calls) != 0 {
		t.Errorf("external schedule should not be recreated, got %v", calls)
	}
	if got := mock.getSchedules()[res.ProgramID].Options.Priority; got != 5 {
		t.Errorf("external schedule priority changed: got %d, want unchanged 5", got)
	}
}

// MaxRecreatesPerPass を超えた分は次のパスに持ち越されること。
func TestReconciler_MaxRecreatesPerPassCarriesOver(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res1 := createReservation(t, ctx, q, 100000500019008, "1件目", startAt)
	res2 := createReservation(t, ctx, q, 100000500019108, "2件目", startAt)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{MaxRecreatesPerPass: 1})
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (create): %v", err)
	}

	setPriorityOverride(t, ctx, q, res1, 31)
	setPriorityOverride(t, ctx, q, res2, 32)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (recreate pass 1): %v", err)
	}

	// res1.ID < res2.ID（挿入順）のため、決定的な順序付け（reservation id 昇順）
	// で res1 が先に処理される。
	schedules := mock.getSchedules()
	if got := schedules[res1.ProgramID].Options.Priority; got != 31 {
		t.Errorf("res1 priority after pass1 = %d, want 31 (processed first)", got)
	}
	if got := schedules[res2.ProgramID].Options.Priority; got != 10 {
		t.Errorf("res2 priority after pass1 = %d, want unchanged 10 (carried over to next pass)", got)
	}

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (recreate pass 2): %v", err)
	}
	schedules = mock.getSchedules()
	if got := schedules[res2.ProgramID].Options.Priority; got != 32 {
		t.Errorf("res2 priority after pass2 = %d, want 32 (recreated on the carried-over pass)", got)
	}
}

// 再作成の DELETE がサーキットブレーカーの削除数に数えられないこと
// （MaxDeletesPerPass を小さくしても再作成自体は走ること）。
func TestReconciler_RecreateDeleteNotCountedByCircuitBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res1 := createReservation(t, ctx, q, 100000500019009, "breaker1", startAt)
	res2 := createReservation(t, ctx, q, 100000500019209, "breaker2", startAt)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{MaxDeletesPerPass: 1})
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (create): %v", err)
	}

	setPriorityOverride(t, ctx, q, res1, 61)
	setPriorityOverride(t, ctx, q, res2, 62)

	tripsBefore := promtestutil.ToFloat64(metrics.ReconcileCircuitBreakerTrips)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (recreate): %v", err)
	}

	if tripsAfter := promtestutil.ToFloat64(metrics.ReconcileCircuitBreakerTrips); tripsAfter != tripsBefore {
		t.Errorf("circuit breaker tripped by recreate deletes: before=%v after=%v (MaxDeletesPerPass=1 with 2 recreates)",
			tripsBefore, tripsAfter)
	}

	schedules := mock.getSchedules()
	if got := schedules[res1.ProgramID].Options.Priority; got != 61 {
		t.Errorf("res1 not recreated despite MaxDeletesPerPass=1: priority=%d, want 61", got)
	}
	if got := schedules[res2.ProgramID].Options.Priority; got != 62 {
		t.Errorf("res2 not recreated despite MaxDeletesPerPass=1: priority=%d, want 62", got)
	}
}

// DELETE 成功 → POST 失敗のとき、メトリクスとエラーログが出て、次のパスで
// 再作成される（レベルトリガーでの復帰）こと。
func TestReconciler_ScheduleLostOnRecreatePostFailure(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019010, "post失敗", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (create): %v", err)
	}

	setPriorityOverride(t, ctx, q, res, 88)
	mock.setFailPostOnce(res.ProgramID)

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	lostBefore := promtestutil.ToFloat64(metrics.ReconcileScheduleLost)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (recreate, injected POST failure): %v", err)
	}

	if lostAfter := promtestutil.ToFloat64(metrics.ReconcileScheduleLost); lostAfter != lostBefore+1 {
		t.Errorf("ReconcileScheduleLost = %v, want %v (DELETE succeeded, POST failed)", lostAfter, lostBefore+1)
	}
	if logText := logBuf.String(); !bytes.Contains([]byte(logText), []byte("level=ERROR")) ||
		!bytes.Contains([]byte(logText), []byte("schedule lost")) {
		t.Errorf("expected an ERROR log mentioning the lost schedule, got:\n%s", logText)
	}

	if _, ok := mock.getSchedules()[res.ProgramID]; ok {
		t.Fatal("schedule should be gone after a delete-succeeded/post-failed recreate")
	}

	// レベルトリガー: 次のパスで再作成されること。
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (recovery): %v", err)
	}
	s, ok := mock.getSchedules()[res.ProgramID]
	if !ok {
		t.Fatal("schedule not recreated on the following pass (level-triggered recovery failed)")
	}
	if s.Options.Priority != 88 {
		t.Errorf("recovered schedule priority = %d, want 88", s.Options.Priority)
	}
}

// --- detached/orphaned の同期対象判定（M2-4 で修正したバグの回帰テスト）---
//
// docs/schema.md §3「state を『mirakc への同期対象か』のフィルタに使っては
// ならない」、docs/recording.md §4.3。同期の可否を決めるのは effective.skip
// であり、state で除外してよいのは orphaned だけ。

// setReservationState は state を直接書き換える。ruler を経由せず、reconciler の
// 同期対象判定だけを state 別に固定したいので生 SQL で任意の state を作る。
func setReservationState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, state string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE reservations SET state = $1 WHERE id = $2`, state, id); err != nil {
		t.Fatalf("setting reservation state to %q: %v", state, err)
	}
}

// 受け入れ基準 14: state='detached' の予約にも schedule が作られる。
//
// 旧 ListActiveReservationsBySite は state = 'active' でしか絞っておらず、
// detached（「実質 manual として動く」はずの状態）の予約が同期対象から
// 落ちていた。手動予約 → たまたまルールがマッチ（active）→ そのルールを
// 編集して外す（detached）という経路で、ユーザーの手動予約が黙って
// 録画されなくなるバグの本体（M2-4 で修正）。
func TestReconciler_DetachedReservationGetsScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019200, "detached予約", startAt)
	setReservationState(t, ctx, pool, res.ID, db.ReservationStateDetached)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := mock.getSchedules()[res.ProgramID]; !ok {
		t.Error("detached reservation should still get a schedule created " +
			"(state must not be used as the sync-target filter; effective.skip decides)")
	}
}

// 受け入れ基準 15: state='orphaned' の予約には schedule が作られない
// （番組終了後に schedule が観測されなかった行なので、作る意味がない）。
func TestReconciler_OrphanedReservationNotScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019201, "orphaned予約", startAt)
	setReservationState(t, ctx, pool, res.ID, db.ReservationStateOrphaned)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := mock.getSchedules()[res.ProgramID]; ok {
		t.Error("orphaned reservation should not get a schedule created")
	}
}

// 受け入れ基準 16: detached かつ intent{skip} の予約には schedule が作られない
// （state だけでなく effective.skip が引き続き効くこと）。
func TestReconciler_DetachedSkipReservationNotScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019202, "detachedスキップ", startAt)
	setReservationState(t, ctx, pool, res.ID, db.ReservationStateDetached)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:              "default",
		ProgramID:         res.ProgramID,
		ProgramStartAt:    res.ProgramStartAt,
		ProgramDurationMs: res.ProgramDurationMs,
	}); err != nil {
		t.Fatalf("recording skip intent: %v", err)
	}

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := mock.getSchedules()[res.ProgramID]; ok {
		t.Error("detached reservation with a skip intent should not get a schedule created")
	}
}
