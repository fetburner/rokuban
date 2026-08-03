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

	"github.com/fetburner/rokuban/internal/breaker"
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

func ptrString(v string) *string { return &v }

// createReservation は networkID=10000/serviceID=5000/GR/27 のチャンネル
// スナップショット（contentpath_test.go と同じ値）を持つ手動予約を作る。
//
// #27 で番組の事実のスナップショットは program_snapshots に抽出された。
// reservations への FK があるため、予約行より先に program_snapshots を upsert
// する（api.CreateReservation と同じ順序。internal/api/handler.go 参照）。
//
// event_id / service_name は issue #98 で program_snapshots に追加された列
// （00025）。recordNeverScheduled が recordings 行を作るのに必須なので、
// この共通ヘルパで必ず埋める。event_id は programID の下 5 桁から機械的に
// 割り当てる --- 本番コードでの programId 分解（不変条件: mirakc の
// programId 内部構造を割り算しない）とは別物で、テストフィクスチャが
// 呼び出しごとに一意な event_id を用意する便宜（同じテスト内で複数の
// createReservation を呼んでも recordings_unique_active_event が衝突しない
// ようにするため）。
func createReservation(t *testing.T, ctx context.Context, q *sqlcgen.Queries, programID int64, title string, startAt time.Time) sqlcgen.Reservation {
	t.Helper()
	networkID, serviceID := int32(10000), int32(5000)
	channelType, channel := "GR", "27"
	eventID := int32(programID % 100000)
	serviceName := "テスト局"
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        "default",
		ProgramID:   programID,
		Title:       title,
		StartAt:     startAt,
		DurationMs:  1800000,
		NetworkID:   networkID,
		ServiceID:   serviceID,
		ChannelType: channelType,
		Channel:     channel,
		EventID:     eventID,
		ServiceName: serviceName,
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	res, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      "default",
		ProgramID: programID,
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
		Site:      "default",
		ProgramID: res.ProgramID,
		Overrides: overrides,
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
	// reconciler は program_snapshots のスナップショットのみを見るので、テストの
	// フィクスチャもスナップショットさせておく必要がある（もう programId からの
	// 算術には頼らない）。
	res := createReservation(t, ctx, q, 100000500011234, "テスト番組", startAt)

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

	tagProgramID, found := mirakc.FindProgramTag(s.Tags)
	if !found {
		t.Fatal("program tag not found in schedule tags")
	}
	if tagProgramID != res.ProgramID {
		t.Errorf("tag program ID = %d, want %d", tagProgramID, res.ProgramID)
	}
	if s.Options.ContentPath == nil || *s.Options.ContentPath == "" {
		t.Error("contentPath is empty")
	}
}

// TestReconciler_DeletesOrphanedSchedule は、desired にまだ生きている予約が
// 残っている状態で（= 全損シグネチャに当たらない）、それとは別の stale な
// schedule が普通に削除されることを確かめる。
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
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019700, "生存中の予約", startAt)

	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if _, ok := schedules[999]; ok {
		t.Errorf("orphaned schedule should have been deleted, got %v", schedules)
	}
	if _, ok := schedules[res.ProgramID]; !ok {
		t.Error("the still-desired reservation's schedule should have been created")
	}
	if len(schedules) != 1 {
		t.Errorf("expected exactly 1 schedule after cleanup, got %d: %v", len(schedules), schedules)
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
	q := sqlcgen.New(pool)

	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if len(schedules) != 1 {
		t.Errorf("non-rokuban schedule should not be deleted, got %d", len(schedules))
	}

	// 外部産の schedule は「自分が作ったもの」ではないので、reservations が
	// 0 件であっても全損シグネチャには当たらない（tag が無いので toDelete
	// にすら入らない）。
	if _, err := q.GetCircuitBreaker(ctx, sqlcgen.GetCircuitBreakerParams{
		Site: "default", Name: breaker.ReconcileTotalLoss,
	}); err == nil {
		t.Error("an external (non-tagged) schedule alone should not trip the total-loss breaker")
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
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        "default",
		ProgramID:   200000500011234,
		Title:       "スキップ番組",
		StartAt:     startAt,
		DurationMs:  1800000,
		NetworkID:   32736,
		ServiceID:   1024,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     1,
		ServiceName: "テスト局",
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      "default",
		ProgramID: 200000500011234,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:      "default",
		ProgramID: 200000500011234,
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

// TestReconciler_NullServiceIDNotScheduled（service_id が NULL の予約を
// 推測せずスキップする回帰テスト）は issue #101（00026）で
// program_snapshots のチャンネル・イベント識別 6 列が NOT NULL 化されたことで
// 削除した。この状態（UpsertProgramSnapshot に channel snapshot を渡さず
// network_id 等が NULL のまま作る）自体が DB レベルで表現不可能になったため
// （INSERT が 23502 で落ちる）。
//
// resolveContentPath / recordNeverScheduled がこの分岐を落とした後も
// 通常の識別子を使う経路が従来どおり動くことは、このファイルの他のテスト
// （TestReconciler_RecreatesOnPriorityChange 等、createReservation ヘルパーで
// 6 列すべてを埋めている）が回帰テストとして機能し続ける。
// NOT NULL が実際に効いていることの回帰テストは internal/db/models_test.go の
// TestSchemaV1_ProgramSnapshotChannelIdentityNotNull にある。

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
	tagProgramID, found := mirakc.FindProgramTag(got.Tags)
	if !found || tagProgramID != res.ProgramID {
		t.Errorf("tag after recreate = %v, want program id %d", got.Tags, res.ProgramID)
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
		Tags:    []string{mirakc.ProgramTag(res.ProgramID)},
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
				Tags:    []string{mirakc.ProgramTag(res.ProgramID)},
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
	staleTag := mirakc.ProgramTag(res.ProgramID + 999999)
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
	tagProgramID, found := mirakc.FindProgramTag(got.Tags)
	if !found || tagProgramID != res.ProgramID {
		t.Errorf("tag after recreate = %v, want program id %d", got.Tags, res.ProgramID)
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

// 再作成の DELETE が全損シグネチャのサーキットブレーカーに一切影響しないこと
// （reservations が生きている間は再作成自体が普通に走ること）。
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

	rec := reconciler.New("default", mc, pool, nil)
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
		t.Errorf("circuit breaker tripped by recreate deletes: before=%v after=%v (recreate DELETEs must not be counted by the total-loss breaker)",
			tripsBefore, tripsAfter)
	}

	schedules := mock.getSchedules()
	if got := schedules[res1.ProgramID].Options.Priority; got != 61 {
		t.Errorf("res1 not recreated: priority=%d, want 61", got)
	}
	if got := schedules[res2.ProgramID].Options.Priority; got != 62 {
		t.Errorf("res2 not recreated: priority=%d, want 62", got)
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

// --- detached/never-scheduled の同期対象判定（M2-4 / issue #98 で修正した
// バグの回帰テスト）---
//
// docs/schema.md §3「state を『mirakc への同期対象か』のフィルタに使っては
// ならない」、docs/recording.md §4.3。同期の可否を決めるのは effective.skip
// であり、除外してよいのは「既に never-scheduled の recordings 行がある」
// 予約だけ（旧 orphaned。issue #98 で reservations.orphaned_at という列自体が
// 廃止され、recordings の試行行の存在に置き換わった）。

// setReservationDetached は ruler を経由せず、reconciler の同期対象判定だけを
// detached 別に固定するため、detached の導出条件（rule_id IS NULL AND base IS
// NOT NULL。#28/#30 で state 列が撤去され、この式からの導出になった）を
// 直接満たす行を生 SQL で作る。
func setReservationDetached(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE reservations SET rule_id = NULL, base = '{}'::jsonb WHERE id = $1`, id); err != nil {
		t.Fatalf("setting reservation to detached: %v", err)
	}
}

// seedNeverScheduledRecording は reconciler.recordNeverScheduled を経由せず、
// その関数が書く recordings 行（status='failed' + quality_events に
// recording.never-scheduled のマーカー）を直接 INSERT する。
// ListReservationsForSyncEvaluation の never-scheduled 除外の判定だけを
// 単体で固定したいテストで使う（旧 setReservationOrphaned の置き換え）。
// createReservation と同じ規約（networkID=10000/serviceID=5000、event_id =
// programID % 100000）でチャンネル識別を合わせるので、同じ res から作った
// program_snapshot の行と (site, network_id, service_id, event_id) が一致する。
func seedNeverScheduledRecording(t *testing.T, ctx context.Context, pool *pgxpool.Pool, res sqlcgen.Reservation) {
	t.Helper()
	eventID := res.ProgramID % 100000
	qe := `[{"at":"2026-01-01T00:00:00Z","event":"recording.never-scheduled","reason":{}}]`
	if _, err := pool.Exec(ctx, `
INSERT INTO recordings (
    reservation_id, source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, program_start_at, program_duration_ms, status, quality_events
) VALUES ($1, 'manual', 'default', 10000, 5000, $2, 'テスト局', 'GR', '27', 'テスト番組', now(), 1800000, 'failed', $3::jsonb)`,
		res.ID, eventID, qe); err != nil {
		t.Fatalf("seeding never-scheduled recording: %v", err)
	}
}

// hasNeverScheduledRecording は指定の reservation に never-scheduled の
// recordings 行があるかどうかを返す（reconciler.recordNeverScheduled が実際に
// 書いたかどうかの確認に使う。internal/db/queries/reservations.sql の
// never-scheduled 除外述語と同じ判定）。
func hasNeverScheduledRecording(t *testing.T, ctx context.Context, pool *pgxpool.Pool, reservationID int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM recordings rec
    WHERE rec.reservation_id = $1
      AND rec.status = 'failed'
      AND EXISTS (
          SELECT 1 FROM jsonb_array_elements(rec.quality_events) qe
          WHERE qe->>'event' = 'recording.never-scheduled'
      )
)`, reservationID).Scan(&exists); err != nil {
		t.Fatalf("checking never-scheduled recording: %v", err)
	}
	return exists
}

// countRecordingsForReservation は指定の reservation に紐づく recordings 行数を
// 返す。冪等性（同じパスを 2 回回しても 2 行目を作らないこと。CLAUDE.md
// 不変条件 5）の確認に使う。
func countRecordingsForReservation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, reservationID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings WHERE reservation_id = $1`, reservationID).Scan(&n); err != nil {
		t.Fatalf("counting recordings for reservation %d: %v", reservationID, err)
	}
	return n
}

// 受け入れ基準 14: detached（rule_id IS NULL AND base IS NOT NULL）の予約にも
// schedule が作られる。
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
	setReservationDetached(t, ctx, pool, res.ID)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := mock.getSchedules()[res.ProgramID]; !ok {
		t.Error("detached reservation should still get a schedule created " +
			"(state must not be used as the sync-target filter; effective.skip decides)")
	}
}

// 受け入れ基準 15: 既に never-scheduled の recordings 行がある予約には
// schedule が作られない（旧「orphaned_at が非 NULL」の同等物。issue #98 で
// 除外条件が recordings の存在に置き換わった）。
func TestReconciler_ReservationWithNeverScheduledRecordingNotRescheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019201, "never-scheduled予約", startAt)
	seedNeverScheduledRecording(t, ctx, pool, res)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := mock.getSchedules()[res.ProgramID]; ok {
		t.Error("a reservation with an existing never-scheduled recording should not get a schedule created")
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
	setReservationDetached(t, ctx, pool, res.ID)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:      "default",
		ProgramID: res.ProgramID,
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

// 回帰テスト: 旧 MarkReservationOrphaned は WHERE id = $1 AND state = 'active' で
// 絞っていたため、detached の予約は番組終了後も orphaned にならなかった
// （終了済みの schedule 無し予約が ListSyncableReservationsBySite に state <>
// 'orphaned' として居座り続け、GC まで desired に残る／schedule が無いので
// 毎パス createSchedule が終了済み番組へ空振りし続ける）。#28/#30 で
// orphaned_at IS NULL に修正され、issue #98 で orphaned_at 自体が recordings
// の試行行に置き換わった今も、active・detached のどちらの予約も番組終了 +
// schedule 観測なしで never-scheduled の recordings 行が作られることを確認する。
func TestReconciler_RecordsNeverScheduledForBothActiveAndDetachedReservations(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 番組は既に終了しており（開始 -2h、30分番組）、mirakc に対応する schedule
	// も無い（mockMirakc は空で始まる）状態を作る。
	endedStartAt := time.Now().Add(-2 * time.Hour)

	activeRes := createReservation(t, ctx, q, 100000500019210, "終了済みactive予約", endedStartAt)
	// createReservation は rule_id/base とも NULL の「active」相当で作る想定
	// なのでそのまま使う。

	detachedRes := createReservation(t, ctx, q, 100000500019211, "終了済みdetached予約", endedStartAt)
	setReservationDetached(t, ctx, pool, detachedRes.ID)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if !hasNeverScheduledRecording(t, ctx, pool, activeRes.ID) {
		t.Error("active reservation has no never-scheduled recording, want one " +
			"(ended program with no observed schedule must produce a never-scheduled recording)")
	}
	if !hasNeverScheduledRecording(t, ctx, pool, detachedRes.ID) {
		t.Error("detached reservation has no never-scheduled recording, want one " +
			"(detached must not be excluded — this was the #30 症状 2 bug: " +
			"MarkReservationOrphaned only matched state = 'active')")
	}
}

// --- 作成ループの終了済み番組ガード（issue #134）---
//
// reconciler の作成ループは終了済み番組の予約にも createSchedule を呼んでいた
// ため、mirakc が数秒で need-rescheduling として failed にし、recordings に
// content_length=0 の failed 行を量産していた（実測: record_sync 46 件中 41
// 件が failed）。ガードは recordNeverScheduled と同じ式・同じ材料
// （program_snapshots の StartAt + DurationMs と now の比較）を使う
// programEnded で行い、境界（終了時刻ちょうどは「終了していない」側）も
// recordNeverScheduled に揃える。

// 受け入れ基準: 終了時刻を過ぎた番組の予約には POST /api/recording/schedules
// が呼ばれず、同じパスでその予約に never-scheduled の recordings 行が
// 作られること。
func TestReconciler_EndedProgramNotScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 開始 -2h、30分番組 → 終了は 90 分前。mirakc に対応する schedule も無い
	// （mockMirakc は空で始まる）ので、ガードが無ければ create ループが
	// createSchedule を呼ぶはずの状況を作る。
	endedStartAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019220, "終了済み番組", endedStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if n := countInt64(mock.getPostCalls(), res.ProgramID); n != 0 {
		t.Errorf("POST /api/recording/schedules called %d times for an ended program, want 0", n)
	}
	if _, ok := mock.getSchedules()[res.ProgramID]; ok {
		t.Error("ended program should not get a schedule created")
	}
	if !hasNeverScheduledRecording(t, ctx, pool, res.ID) {
		t.Error("no never-scheduled recording found, want one " +
			"(the same pass's recordNeverScheduled must record the ended reservation as never-scheduled, " +
			"using the same programEnded predicate as the create-loop guard)")
	}
}

// 受け入れ基準（反転）: 放送中の番組・未来の番組では従来どおり POST される
// こと。ガードの条件は「終了済み」だけで「開始済み」ではない —— mirakc は
// 放送中の番組の schedule を受け付けて途中から録る（issue #134 の実測:
// 23:10 開始の番組を 23:16:39 に予約 → 正常に録画継続、426MB 到達）。
// 「開始済み」で切ると、ユーザーが放送中の番組を予約する経路が黙って壊れる。
func TestReconciler_BroadcastingProgramStillScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 開始 6 分前、30 分番組 → まだ 24 分残っている「放送中」の状態。
	broadcastingStartAt := time.Now().Add(-6 * time.Minute)
	res := createReservation(t, ctx, q, 100000500019221, "放送中番組", broadcastingStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if n := countInt64(mock.getPostCalls(), res.ProgramID); n != 1 {
		t.Errorf("POST /api/recording/schedules called %d times for a broadcasting program, want 1", n)
	}
	if _, ok := mock.getSchedules()[res.ProgramID]; !ok {
		t.Error("broadcasting program should still get a schedule created")
	}
	if hasNeverScheduledRecording(t, ctx, pool, res.ID) {
		t.Error("unexpected never-scheduled recording (program is still broadcasting)")
	}
}

// 未来の番組（開始前）でも同様に POST されることを確認する
// （TestReconciler_CreatesSchedule と重複する観点だが、この節の反転テストと
// して programEnded ガードの境界を明示するために置く）。
func TestReconciler_FutureProgramStillScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	futureStartAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019222, "未来の番組", futureStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if n := countInt64(mock.getPostCalls(), res.ProgramID); n != 1 {
		t.Errorf("POST /api/recording/schedules called %d times for a future program, want 1", n)
	}
	if hasNeverScheduledRecording(t, ctx, pool, res.ID) {
		t.Error("unexpected never-scheduled recording (program has not started yet)")
	}
}

// --- never-scheduled 行の冪等性・#59 の解消・GC 後の生存（issue #98）---

// 受け入れ基準: 番組終了時に schedule が観測されなかった予約について、
// never-scheduled の recordings 行が 1 行だけ作られる。2 回パスを回しても
// 2 行目が作られないこと（CLAUDE.md 不変条件 5: レベルトリガーは冪等でなければ
// ならない）。
func TestReconciler_NeverScheduledRecordingIsIdempotentAcrossPasses(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	endedStartAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019230, "終了済み番組（冪等性）", endedStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}
	if got := countRecordingsForReservation(t, ctx, pool, res.ID); got != 1 {
		t.Fatalf("recordings count after 1st pass = %d, want 1", got)
	}

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}
	if got := countRecordingsForReservation(t, ctx, pool, res.ID); got != 1 {
		t.Errorf("recordings count after 2nd pass = %d, want 1 (2 回目のパスで 2 行目を作ってはならない)", got)
	}
}

// SQL レベルの冪等性: CreateNeverScheduledRecording を全く同じ入力で 2 回
// 呼んでも 2 行目を作らない（ON CONFLICT ... DO NOTHING の直接確認）。
//
// TestReconciler_NeverScheduledRecordingIsIdempotentAcrossPasses（reconciler
// 経由）は、1 回目のパスで作った never-scheduled 行のせいで
// ListReservationsForSyncEvaluation の除外フィルタが 2 回目のパスの時点で
// その予約自体を listDesired から除いてしまうため、実は
// CreateNeverScheduledRecording 自身が 2 回呼ばれる状況を再現できていない
// （呼び出しが 1 回しか起きない）。クエリ自身の防御を単体で確認するには
// q を直接、同じ入力で 2 回叩く必要がある。
func TestCreateNeverScheduledRecording_IdempotentOnDirectRetry(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019240, "直接冪等性確認", startAt)

	params := sqlcgen.CreateNeverScheduledRecordingParams{
		ReservationID:     &res.ID,
		Source:            db.SourceManual,
		Site:              "default",
		NetworkID:         10000,
		ServiceID:         5000,
		EventID:           int32(res.ProgramID % 100000),
		ServiceName:       "テスト局",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "直接冪等性確認",
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
		QualityEvents:     json.RawMessage(`[{"at":"2026-01-01T00:00:00Z","event":"recording.never-scheduled","reason":{}}]`),
	}
	if _, err := q.CreateNeverScheduledRecording(ctx, params); err != nil {
		t.Fatalf("1st CreateNeverScheduledRecording: %v", err)
	}
	if _, err := q.CreateNeverScheduledRecording(ctx, params); err != nil {
		t.Fatalf("2nd CreateNeverScheduledRecording: %v", err)
	}

	if got := countRecordingsForReservation(t, ctx, pool, res.ID); got != 1 {
		t.Errorf("recordings count after 2 identical calls = %d, want 1 "+
			"(ON CONFLICT ... DO NOTHING must make the 2nd call a no-op)", got)
	}
}

// 受け入れ基準（#59 本体の解消）: 既に成功した録画（recordings.status が
// 'recording'/'finished' 等）がある放送イベントについては、schedule が
// 同期対象の一覧から見えなくても never-scheduled 行を作らない。
//
// 旧 markOrphaned は reservations の schedule 観測だけを見て recordings を
// 一切見ていなかったため、mirakc が録画完了後に schedule を消す（#134 の
// 実測）と、成功した録画の予約まで「orphaned」（= 録れなかった）と誤判定して
// いた。#98 の書き込み条件「その放送イベントに生きている recordings 行が
// 無いこと」（ON CONFLICT DO NOTHING）がこれを解消する。
func TestReconciler_SuccessfulRecordingPreventsNeverScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	endedStartAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019231, "成功済み録画の予約", endedStartAt)

	// watcher が既に成功録画を記録済みの状態を模す。createReservation の規約
	// （networkID=10000/serviceID=5000/event_id=programID%100000）と同じ
	// active-event を占有させる。
	eventID := res.ProgramID % 100000
	var recordingID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO recordings (
    reservation_id, source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, program_start_at, program_duration_ms, status,
    started_at, ended_at
) VALUES ($1, 'manual', 'default', 10000, 5000, $2, 'テスト局', 'GR', '27', '成功済み録画の予約',
    $3::timestamptz, 1800000, 'finished', $3::timestamptz, $3::timestamptz + interval '30 minutes')
RETURNING id`,
		res.ID, eventID, endedStartAt).Scan(&recordingID); err != nil {
		t.Fatalf("seeding successful recording: %v", err)
	}

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := countRecordingsForReservation(t, ctx, pool, res.ID); got != 1 {
		t.Errorf("recordings count = %d, want 1 (a never-scheduled row must not be created "+
			"alongside an already-successful recording — this is #59)", got)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM recordings WHERE id = $1`, recordingID).Scan(&status); err != nil {
		t.Fatalf("reading recording status: %v", err)
	}
	if status != "finished" {
		t.Errorf("recording status = %q, want %q (the successful recording must not be touched)", status, "finished")
	}
}

// 受け入れ基準（#98 帰結 2 の解消）: ルールを削除して予約行が導出削除されても
// （FK の ON DELETE SET NULL を経由せず、api.DeleteReservationsByRuleWithoutIntent
// のような物理削除を経路として想定）、never-scheduled の recordings 行は
// 残る --- recordings.reservation_id は ON DELETE SET NULL であって
// recordings 行自体は消えない（docs/schema.md §3.7「recordings はこの FK の
// 対象外」）。ここでは reservations 行を直接 DELETE して同じ経路を再現する
// （rule 削除の API を経由しなくても、reservations の削除経路はすべてこの
// FK の挙動に帰着するため）。
func TestReconciler_NeverScheduledRecordingSurvivesReservationDeletion(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	endedStartAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019232, "ルール削除で消える予約", endedStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if !hasNeverScheduledRecording(t, ctx, pool, res.ID) {
		t.Fatalf("precondition: never-scheduled recording must exist before deleting the reservation")
	}

	// ルール削除に伴う導出予約の物理削除を直接模す（internal/api/rules.go の
	// DeleteReservationsByRuleWithoutIntent と同じ「reservations 行の DELETE」
	// という結果だけを再現する）。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE id = $1`, res.ID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings WHERE site = 'default' AND network_id = 10000 AND service_id = 5000 AND event_id = $1`,
		res.ProgramID%100000).Scan(&count); err != nil {
		t.Fatalf("counting recordings: %v", err)
	}
	if count != 1 {
		t.Errorf("recordings count after reservation deletion = %d, want 1 "+
			"(the never-scheduled recording must survive the derived deletion of its reservation)", count)
	}
	var reservationID *int64
	if err := pool.QueryRow(ctx, `SELECT reservation_id FROM recordings WHERE site = 'default' AND network_id = 10000 AND service_id = 5000 AND event_id = $1`,
		res.ProgramID%100000).Scan(&reservationID); err != nil {
		t.Fatalf("reading recording reservation_id: %v", err)
	}
	if reservationID != nil {
		t.Errorf("recording.reservation_id = %v, want nil (ON DELETE SET NULL)", *reservationID)
	}
}

// 受け入れ基準（#98 帰結 1 の解消）: program_snapshots の GC（終了 +
// epg.retention_grace）で予約行が消えても never-scheduled の recordings 行は
// 残る。recordings は program_snapshots への FK を持たない（docs/schema.md
// §3.7「recordings はこの FK の対象外。録画時点のスナップショットとして
// 独立にコピーを持つ」）ので、program_snapshots → reservations の CASCADE が
// 走っても影響を受けない。
func TestReconciler_NeverScheduledRecordingSurvivesProgramSnapshotGC(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	endedStartAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019233, "GC で消える予約", endedStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if !hasNeverScheduledRecording(t, ctx, pool, res.ID) {
		t.Fatalf("precondition: never-scheduled recording must exist before GC")
	}

	// ruler.runGC と同じ 1 本の DELETE（DeleteEndedProgramSnapshots）を直接
	// 呼ぶ。FK の ON DELETE CASCADE で reservations も一緒に落ちる。
	if _, err := q.DeleteEndedProgramSnapshots(ctx, time.Now()); err != nil {
		t.Fatalf("running program_snapshots GC: %v", err)
	}

	var reservationExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reservations WHERE id = $1)`, res.ID).Scan(&reservationExists); err != nil {
		t.Fatalf("checking reservation existence: %v", err)
	}
	if reservationExists {
		t.Fatalf("precondition: GC must have deleted the reservation row")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings WHERE site = 'default' AND network_id = 10000 AND service_id = 5000 AND event_id = $1`,
		res.ProgramID%100000).Scan(&count); err != nil {
		t.Fatalf("counting recordings: %v", err)
	}
	if count != 1 {
		t.Errorf("recordings count after program_snapshots GC = %d, want 1 "+
			"(the never-scheduled recording must survive the GC of program_snapshots/reservations)", count)
	}
}

// 受け入れ基準（#134 の挙動を壊していないこと）: never-scheduled の
// recordings 行が作られた予約は、次のパスで desired に戻って POST を
// 撃ち直されない。ListReservationsForSyncEvaluation の除外条件が
// 「never-scheduled の recordings 行が無いこと」になったことの直接確認。
func TestReconciler_EndedReservationNotReAddedToDesiredOnNextPass(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	endedStartAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019234, "2 パス目でも desired に戻らない番組", endedStartAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}
	if n := countInt64(mock.getPostCalls(), res.ProgramID); n != 0 {
		t.Fatalf("POST called %d times on 1st pass, want 0", n)
	}

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}
	if n := countInt64(mock.getPostCalls(), res.ProgramID); n != 0 {
		t.Errorf("POST /api/recording/schedules called %d times across 2 passes, want 0 "+
			"(the ended reservation must not re-enter desired and get POSTed again — issue #134)", n)
	}
}

// スキップ件数のログ（stateGuarded / limitCarriedOver と同じ扱い）が出ること。
func TestReconciler_LogsEndedProgramSkipCount(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	endedStartAt := time.Now().Add(-2 * time.Hour)
	createReservation(t, ctx, q, 100000500019223, "終了済み番組（ログ確認）", endedStartAt)

	rec := reconciler.New("default", mc, pool, nil)

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	logText := logBuf.Bytes()
	if !bytes.Contains(logText, []byte("level=INFO")) || !bytes.Contains(logText, []byte("program already ended")) {
		t.Errorf("expected an INFO log about skipped ended-program create candidates, got:\n%s", logText)
	}
	if !bytes.Contains(logText, []byte("count=1")) {
		t.Errorf("expected the skip log to report count=1, got:\n%s", logText)
	}
}

// --- 全損シグネチャ・サーキットブレーカー（breaker.ReconcileTotalLoss、issue #24 M2-5）---
//
// 件数ベースの MaxDeletesPerPass は撤去した（reconciler からは ruler の導出削除・
// ユーザーの明示操作・GC のどれで desired が減ったのか区別できず、後の 2 つで
// 誤発火するだけだったため。docs/recording.md §3.2、issue #2 の M2-5 決定コメント）。
// 代わりに「desired（reservations）が 1 件もないのに、自分が作った schedule が
// 観測されている」という全損シグネチャだけを守る。

// 受け入れ基準 1・7: 予約が 1 件もなく、自分の tag が付いた schedule が観測されたら
// 削除せず発動し、detail に消されようとしていた schedule の抜粋（programId と
// mirakc から観測した番組名）が入ること。
func TestReconciler_TotalLossSignatureTripsAndWithholdsDeletes(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	mock := newMockMirakc()
	mock.schedules[999] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 999, Name: ptrString("全損番組")},
		Tags:    []string{"rokuban:reservation=9999"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if schedules := mock.getSchedules(); len(schedules) != 1 {
		t.Errorf("total-loss signature should withhold the delete, got %d schedules: %v", len(schedules), schedules)
	}

	cb, err := q.GetCircuitBreaker(ctx, sqlcgen.GetCircuitBreakerParams{
		Site: "default", Name: breaker.ReconcileTotalLoss,
	})
	if err != nil {
		t.Fatalf("expected the total-loss breaker to be tripped, GetCircuitBreaker: %v", err)
	}
	if cb.Pending != 1 {
		t.Errorf("pending = %d, want 1", cb.Pending)
	}

	var sample breaker.Sample
	if err := json.Unmarshal(cb.Detail, &sample); err != nil {
		t.Fatalf("unmarshalling detail: %v", err)
	}
	if sample.Total != 1 {
		t.Errorf("sample.Total = %d, want 1", sample.Total)
	}
	if len(sample.Programs) != 1 || sample.Programs[0].ProgramID != 999 {
		t.Fatalf("sample.Programs = %+v, want a single entry for program 999", sample.Programs)
	}
	if sample.Programs[0].Title != "全損番組" {
		t.Errorf("sample.Programs[0].Title = %q, want %q", sample.Programs[0].Title, "全損番組")
	}
}

// 受け入れ基準 2: 予約が 1 件でも残っていれば、削除数がいくら多くても（旧閾値
// MaxDeletesPerPass=10 を超える 11 件を含めても）削除が実行されること。これが
// 件数ベースのブレーカーを撤去した理由そのもの — GC・ユーザー操作による正当な
// 一括削除で誤発火してはならない。
func TestReconciler_BulkDeleteWithRemainingReservationDoesNotTripBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	mock := newMockMirakc()

	// 生き残る予約とその schedule。
	startAt := time.Now().Add(1 * time.Hour)
	res := createReservation(t, ctx, q, 100000500019800, "生存", startAt)
	mock.schedules[res.ProgramID] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: res.ProgramID},
		Tags:    []string{mirakc.ProgramTag(res.ProgramID)},
	}

	// もう desired にない自分のタグ付き schedule を 11 件（旧閾値 10 を超える）。
	// 旧形式のタグのまま（意図的）: IsOurs が新旧どちらの形式も「自分が作った」と
	// 認識することの回帰確認を兼ねる（#53 の移行方針）。
	staleIDs := make([]int64, 0, 11)
	for i := int64(0); i < 11; i++ {
		id := int64(100000500019900) + i
		staleIDs = append(staleIDs, id)
		mock.schedules[id] = mirakc.Schedule{
			State:   "scheduled",
			Program: mirakc.Program{ID: id},
			Tags:    []string{fmt.Sprintf("rokuban:reservation=%d", 900000+i)},
		}
	}

	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	schedules := mock.getSchedules()
	if _, ok := schedules[res.ProgramID]; !ok {
		t.Error("the surviving reservation's schedule should not be deleted")
	}
	for _, id := range staleIDs {
		if _, ok := schedules[id]; ok {
			t.Errorf("stale schedule %d should have been deleted despite exceeding the old MaxDeletesPerPass=10 threshold", id)
		}
	}
	if len(schedules) != 1 {
		t.Errorf("expected only the surviving schedule to remain, got %d: %v", len(schedules), schedules)
	}

	if _, err := q.GetCircuitBreaker(ctx, sqlcgen.GetCircuitBreakerParams{
		Site: "default", Name: breaker.ReconcileTotalLoss,
	}); err == nil {
		t.Error("the total-loss breaker should not trip when a desired reservation remains")
	}
}

// 受け入れ基準 4・5: 発動中は次のパスでも delete が実行されない（ラッチである
// こと）。全損シグネチャがそのパスでは既に見えなくなっていても（＝予約が復活
// していても）、ラッチは自動では解けない。加えて、発動中でも新規予約の
// schedule 作成は続けられること（止めているのは削除だけ）。
func TestReconciler_TotalLossBreakerLatchesAcrossPasses(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	mock := newMockMirakc()
	mock.schedules[999] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 999},
		Tags:    []string{"rokuban:reservation=9999"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st, trips the breaker): %v", err)
	}
	if _, ok := mock.getSchedules()[999]; !ok {
		t.Fatalf("breaker should have withheld the delete on pass 1")
	}

	// シグネチャが消えても（予約が復活しても）ラッチは自動で解除されない。
	startAt := time.Now().Add(1 * time.Hour)
	newRes := createReservation(t, ctx, q, 100000500020000, "復活後の予約", startAt)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd, signature cleared but latch should hold): %v", err)
	}

	schedules := mock.getSchedules()
	if _, ok := schedules[999]; !ok {
		t.Error("latch should still withhold the stale schedule's deletion even though " +
			"the total-loss signature no longer holds this pass")
	}
	if _, ok := schedules[newRes.ProgramID]; !ok {
		t.Error("schedule creation should still happen while the delete-only breaker is latched")
	}
}

// 受け入れ基準 6: ResumeCircuitBreaker で行を消した後のパスでは削除が実行され、
// 収束すること。
func TestReconciler_ResumeCircuitBreakerConverges(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	mock := newMockMirakc()
	mock.schedules[999] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 999},
		Tags:    []string{"rokuban:reservation=9999"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	rec := reconciler.New("default", mc, pool, nil)

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := mock.getSchedules()[999]; !ok {
		t.Fatalf("expected the delete to be withheld before resume")
	}

	// 人間が確認し、desired 側も正常な状態（予約が 1 件以上ある）に復旧した上で
	// 手動再開する、という筋書き。
	startAt := time.Now().Add(1 * time.Hour)
	newRes := createReservation(t, ctx, q, 100000500020100, "復旧後の予約", startAt)

	rows, err := q.ResumeCircuitBreaker(ctx, sqlcgen.ResumeCircuitBreakerParams{
		Site: "default", Name: breaker.ReconcileTotalLoss,
	})
	if err != nil || rows != 1 {
		t.Fatalf("ResumeCircuitBreaker: rows=%d err=%v", rows, err)
	}

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (after resume): %v", err)
	}

	schedules := mock.getSchedules()
	if _, ok := schedules[999]; ok {
		t.Error("the stale schedule should be deleted once the breaker is resumed and a desired reservation exists again")
	}
	if _, ok := schedules[newRes.ProgramID]; !ok {
		t.Error("the recovered reservation's schedule should be created")
	}
}

// --- 開始遅延検出器（issue #24 M2-7、docs/recording.md §3.3「開始遅延検出器」）---
//
// 「開始時刻 + StartDelayGrace を過ぎたのに recordings.started_at が観測されて
// いない予約」を rokuban_reconcile_start_delayed ゲージ（{site} ラベル）と
// slog.Error で検出する。DB に新しい状態は持たせず、毎パス再計算する導出値
// （不変条件 5: レベルトリガー）。

// createStartedRecording は reservationID に紐づく recordings 行を、
// 録画開始が観測済み（started_at が埋まっている）状態で作る。watcher が
// mirakc の record から recordings.started_at を書く経路を模している。
func createStartedRecording(t *testing.T, ctx context.Context, q *sqlcgen.Queries, reservationID int64, eventID int32, programStartAt, startedAt time.Time) {
	t.Helper()
	if _, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		ReservationID:     &reservationID,
		Source:            "manual",
		Site:              "default",
		NetworkID:         10000,
		ServiceID:         5000,
		EventID:           eventID,
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		ProgramStartAt:    programStartAt,
		ProgramDurationMs: 1800000,
		Status:            "recording",
		StartedAt:         &startedAt,
	}); err != nil {
		t.Fatalf("creating started recording: %v", err)
	}
}

// startDelayGauge は metrics.ReconcileStartDelayed の "default" サイトの現在値を読む。
func startDelayGauge(t *testing.T) float64 {
	t.Helper()
	return promtestutil.ToFloat64(metrics.ReconcileStartDelayed.WithLabelValues("default"))
}

// 受け入れ条件 1: 開始時刻 + 猶予を過ぎて recordings.started_at が無い予約が
// 検出されること（観測欠落を注入して検出されること）。ゲージとエラーログの
// 両方に、予約 ID・programId・題名・経過が出ることを確認する。
func TestReconciler_DetectsStartDelay(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 開始 10 分前（既定猶予 3 分を過ぎている）、30 分番組なのでまだ終了前。
	startAt := time.Now().Add(-10 * time.Minute)
	res := createReservation(t, ctx, q, 100000500030001, "開始遅延番組", startAt)

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := startDelayGauge(t); got != 1 {
		t.Errorf("rokuban_reconcile_start_delayed = %v, want 1", got)
	}

	logText := logBuf.Bytes()
	if !bytes.Contains(logText, []byte("level=ERROR")) || !bytes.Contains(logText, []byte("not started")) {
		t.Errorf("expected an ERROR log about the start delay, got:\n%s", logText)
	}
	if !bytes.Contains(logText, []byte(fmt.Sprintf("reservation_id=%d", res.ID))) {
		t.Errorf("expected the log to include reservation_id=%d, got:\n%s", res.ID, logText)
	}
	if !bytes.Contains(logText, []byte(fmt.Sprintf("program_id=%d", res.ProgramID))) {
		t.Errorf("expected the log to include program_id=%d, got:\n%s", res.ProgramID, logText)
	}
	if !bytes.Contains(logText, []byte("開始遅延番組")) {
		t.Errorf("expected the log to include the program title, got:\n%s", logText)
	}
	if !bytes.Contains(logText, []byte("elapsed=")) {
		t.Errorf("expected the log to include the elapsed time since start, got:\n%s", logText)
	}
}

// 受け入れ条件 2: recordings.started_at があれば検出されないこと
// （正常に始まった録画で騒がない）。
func TestReconciler_NoStartDelayWhenStarted(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(-10 * time.Minute)
	res := createReservation(t, ctx, q, 100000500030002, "正常開始番組", startAt)
	createStartedRecording(t, ctx, q, res.ID, 30002, startAt, startAt.Add(1*time.Minute))

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := startDelayGauge(t); got != 0 {
		t.Errorf("rokuban_reconcile_start_delayed = %v, want 0 (recording already started)", got)
	}
}

// 受け入れ条件 3: 猶予の内側（開始直後）では検出されないこと（誤検知しない
// こと。猶予を置いた理由そのもの）。
func TestReconciler_NoStartDelayWithinGrace(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 既定猶予は 3 分。開始 1 分後はまだ猶予の内側で、SSE 到達・watcher 処理の
	// 遅れによる誤検知を防ぐ窓に入っている。
	startAt := time.Now().Add(-1 * time.Minute)
	createReservation(t, ctx, q, 100000500030003, "開始直後番組", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := startDelayGauge(t); got != 0 {
		t.Errorf("rokuban_reconcile_start_delayed = %v, want 0 (still within StartDelayGrace)", got)
	}
}

// 受け入れ条件 4: 番組終了時刻を既に過ぎた予約は検出されないこと
// （recordNeverScheduled の領分。終わった番組を遅延として報告し続けると
// アラートが鳴り止まなくなる）。
func TestReconciler_NoStartDelayAfterProgramEnded(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 開始 40 分前・30 分番組なので、終了時刻（開始 30 分後）はとっくに過ぎている。
	startAt := time.Now().Add(-40 * time.Minute)
	createReservation(t, ctx, q, 100000500030004, "終了済み番組", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := startDelayGauge(t); got != 0 {
		t.Errorf("rokuban_reconcile_start_delayed = %v, want 0 (program already ended; recordNeverScheduled's territory)", got)
	}
}

// 受け入れ条件 5: 既に never-scheduled の recordings 行がある予約は検出され
// ないこと（既に「録れなかった」とマークされている。二重に騒がない）。
// issue #98 で旧「orphaned_at が非 NULL」から置き換わった。
func TestReconciler_NoStartDelayForNeverScheduledReservation(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	// 開始時刻 + 猶予を過ぎているが終了前という、本来なら検出される窓。
	startAt := time.Now().Add(-10 * time.Minute)
	res := createReservation(t, ctx, q, 100000500030005, "never-scheduled開始遅延", startAt)
	seedNeverScheduledRecording(t, ctx, pool, res)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := startDelayGauge(t); got != 0 {
		t.Errorf("rokuban_reconcile_start_delayed = %v, want 0 (never-scheduled reservations must not be double-reported)", got)
	}
}

// 受け入れ条件 6: effective.skip の予約は検出されないこと（録画しない意図
// なのだから始まらないのが正常）。
func TestReconciler_NoStartDelayForSkippedReservation(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(-10 * time.Minute)
	createReservation(t, ctx, q, 100000500030006, "skip開始遅延", startAt)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:      "default",
		ProgramID: 100000500030006,
	}); err != nil {
		t.Fatalf("recording skip intent: %v", err)
	}

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if got := startDelayGauge(t); got != 0 {
		t.Errorf("rokuban_reconcile_start_delayed = %v, want 0 (skip intent means not starting is normal)", got)
	}
}

// 受け入れ条件 7: ゲージが収束すること（次のパスで started_at が埋まったら
// ゼロに戻る。カウンタではなくゲージにした理由の検証）。
func TestReconciler_StartDelayGaugeConverges(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(-10 * time.Minute)
	res := createReservation(t, ctx, q, 100000500030007, "収束確認番組", startAt)

	rec := reconciler.New("default", mc, pool, nil)
	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}
	if got := startDelayGauge(t); got != 1 {
		t.Fatalf("rokuban_reconcile_start_delayed after pass 1 = %v, want 1", got)
	}

	// watcher が録画開始を観測した想定で started_at を埋める。
	createStartedRecording(t, ctx, q, res.ID, 30007, startAt, startAt.Add(4*time.Minute))

	if err := rec.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (2nd): %v", err)
	}
	if got := startDelayGauge(t); got != 0 {
		t.Errorf("rokuban_reconcile_start_delayed after pass 2 = %v, want 0 (converges once started_at is observed)", got)
	}
}
