package ruler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/testutil"
)

const testSite = db.DefaultSite

// テスト全体で使い回す単一のチャンネル。複数チャンネルを区別する必要のある
// テストがないので固定値にする（unparam 対策も兼ねる）。
const (
	testNetworkID int32 = 32736
	testServiceID int32 = 1024
	// testDurationMs はテスト全体で使い回す番組長（30分）。延長シナリオも
	// 開始時刻をずらすだけで長さ自体は変えていないので固定値にする
	// （unparam 対策も兼ねる）。
	testDurationMs int64 = 1800000
)

// insertService は EPG プロジェクションにサービス（チャンネル）行を用意する。
func insertService(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, $2, $3, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, testSite, testNetworkID, testServiceID)
	if err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}
}

// insertProgram は EPG プロジェクションに番組行を作成/更新する。
func insertProgram(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, title string, startAt time.Time) {
	t.Helper()
	endAt := startAt.Add(time.Duration(testDurationMs) * time.Millisecond)
	_, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ($1, $2, $3, $4, 0, $5, $6, $7, true, $8, '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO UPDATE SET
  start_at = EXCLUDED.start_at, duration_ms = EXCLUDED.duration_ms,
  end_at = EXCLUDED.end_at, name = EXCLUDED.name`,
		testSite, programID, testNetworkID, testServiceID, startAt, testDurationMs, endAt, title)
	if err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}
}

// deleteProgram は EPG プロジェクションから番組を削除する（射影の刈り取りを模す）。
func deleteProgram(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM epg_programs WHERE site = $1 AND program_id = $2`, testSite, programID); err != nil {
		t.Fatalf("deleting epg_programs fixture: %v", err)
	}
}

// insertRule は有効な最小構成のルールを作る（enabled はデフォルトの true のまま）。
// 無効化して「マッチしなくなる」状況を作りたいテストは、作成後に
// UPDATE rules SET enabled = false で直接落とす。条件は insertRuleKeyword で別途足す。
func insertRule(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string, priority int32) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
INSERT INTO rules (name, priority) VALUES ($1, $2) RETURNING id`,
		name, priority).Scan(&id)
	if err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	return id
}

// insertRuleKeyword はルールに「番組名に keyword を含む」条件を 1 つ足す。
// case_sensitive にして正規化 (normalize_search_text) の影響を受けない単純な
// 部分一致にする。
func insertRuleKeyword(t *testing.T, pool *pgxpool.Pool, ctx context.Context, ruleID int64, keyword string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, 0, 'name', 'keyword', $2, true, false)`, ruleID, keyword)
	if err != nil {
		t.Fatalf("inserting rule_text_matches fixture: %v", err)
	}
}

// reservationRow はテストで確認したい reservations の列だけを持つ。
type reservationRow struct {
	ID                int64
	RuleID            *int64
	State             string
	Source            string
	Base              []byte
	Title             string
	ProgramStartAt    time.Time
	ProgramDurationMs int64
	NetworkID         *int32
	ServiceID         *int32
	ChannelType       *string
	Channel           *string
	UpdatedAt         time.Time
}

func getReservation(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64) (reservationRow, bool) {
	t.Helper()
	var r reservationRow
	err := pool.QueryRow(ctx, `
SELECT id, rule_id, state, source, base, title, program_start_at, program_duration_ms,
       network_id, service_id, channel_type, channel, updated_at
FROM reservations WHERE site = $1 AND program_id = $2`, testSite, programID).Scan(
		&r.ID, &r.RuleID, &r.State, &r.Source, &r.Base, &r.Title, &r.ProgramStartAt, &r.ProgramDurationMs,
		&r.NetworkID, &r.ServiceID, &r.ChannelType, &r.Channel, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reservationRow{}, false
		}
		t.Fatalf("querying reservation for program %d: %v", programID, err)
	}
	return r, true
}

func basePriority(t *testing.T, raw []byte) *int {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var opts db.ReservationOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		t.Fatalf("unmarshalling base %s: %v", raw, err)
	}
	return opts.Priority
}

// 受け入れ基準 1: ruler は program_intents を一切書かない（案 A の核心）。
func TestRunPass_DoesNotWriteProgramIntents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 1001, "ニュース7", start)

	ruleID := insertRule(t, pool, ctx, "news", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "ニュース")

	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 1001, Action: db.IntentRecord,
		ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	before, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: testSite, ProgramID: 1001})
	if err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	after, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: testSite, ProgramID: 1001})
	if err != nil {
		t.Fatal(err)
	}

	if !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Errorf("program_intents.updated_at changed: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if !before.CreatedAt.Equal(after.CreatedAt) {
		t.Errorf("program_intents.created_at changed: %v -> %v", before.CreatedAt, after.CreatedAt)
	}
	if before.Action != after.Action {
		t.Errorf("program_intents.action changed: %q -> %q", before.Action, after.Action)
	}
}

// 受け入れ基準 4: ruler は program_overrides を一切書かない
// （program_intents と同じ規律。docs/recording.md §4.2「ruler は base だけを書く」）。
func TestRunPass_DoesNotWriteProgramOverrides(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 1101, "上書きテスト", start)

	ruleID := insertRule(t, pool, ctx, "override-write-guard", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "上書きテスト")

	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 1101, Overrides: []byte(`{"priority":9}`),
		ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	before, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: testSite, ProgramID: 1101})
	if err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	after, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: testSite, ProgramID: 1101})
	if err != nil {
		t.Fatal(err)
	}

	if !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Errorf("program_overrides.updated_at changed: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if string(before.Overrides) != string(after.Overrides) {
		t.Errorf("program_overrides.overrides changed: %s -> %s", before.Overrides, after.Overrides)
	}
}

// 受け入れ基準 5: program_overrides に行があるだけで、ルールがマッチしなくなった
// あとも予約が detached として生き残る（削除されない。docs/recording.md §4.2
// 「ruler から見た load-bearing な行」）。
func TestRunPass_OverridesAloneKeepReservationDetached(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 1201, "上書き生存", start)

	ruleID := insertRule(t, pool, ctx, "override-survivor", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "上書き生存")

	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 1201, Overrides: []byte(`{"priority":9}`),
		ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	if _, ok := getReservation(t, pool, ctx, 1201); !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}

	// ルールを無効化 → マッチしなくなるが、program_overrides に行があるので
	// desired から外れない（意図が無くても上書きだけで予約が生き残る）。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after disabling rule: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 1201)
	if !ok {
		t.Fatal("reservation with only program_overrides (no intent) should survive as detached, not be deleted")
	}
	if res.RuleID != nil {
		t.Errorf("rule_id = %v, want nil after detaching", res.RuleID)
	}
	if res.State != db.ReservationStateDetached {
		t.Errorf("state = %q, want %q", res.State, db.ReservationStateDetached)
	}
}

// 受け入れ基準 2: intent{skip} の番組には予約行が作られない。
func TestRunPass_SkipIntentPreventsReservation(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 2001, "映画スペシャル", start)

	ruleID := insertRule(t, pool, ctx, "movie", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "映画")

	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: testSite, ProgramID: 2001, ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := getReservation(t, pool, ctx, 2001); ok {
		t.Error("reservation should not be created for a skip-intent program even though a rule matches it")
	}
}

// 受け入れ基準 3: 全量パスを 2 回連続で回しても 2 回目に UPDATE が発生しない
// （差分書き込みの検証。updated_at で見る）。
func TestRunPass_SecondPassIsNoOp(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 3001, "アニメ", start)
	ruleID := insertRule(t, pool, ctx, "anime", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "アニメ")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first, ok := getReservation(t, pool, ctx, 3001)
	if !ok {
		t.Fatal("reservation not created on first pass")
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second, ok := getReservation(t, pool, ctx, 3001)
	if !ok {
		t.Fatal("reservation missing after second pass")
	}

	if !first.UpdatedAt.Equal(second.UpdatedAt) {
		t.Errorf("updated_at changed on a no-diff second pass: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
}

// 受け入れ基準 4: priority の異なる複数ルールがマッチしたら高い方の base が入り、
// 同率なら id の小さい方が勝つ（全順序 priority DESC, id ASC）。
func TestRunPass_WinnerByPriorityThenID(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 4001, "対象番組", start)

	lowID := insertRule(t, pool, ctx, "low", 5)
	insertRuleKeyword(t, pool, ctx, lowID, "対象番組")
	highID := insertRule(t, pool, ctx, "high", 20)
	insertRuleKeyword(t, pool, ctx, highID, "対象番組")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 4001)
	if !ok {
		t.Fatal("reservation not created")
	}
	if res.RuleID == nil || *res.RuleID != highID {
		t.Errorf("rule_id = %v, want %d (higher priority rule)", res.RuleID, highID)
	}
	if p := basePriority(t, res.Base); p == nil || *p != 20 {
		t.Errorf("base priority = %v, want 20", p)
	}

	// 同率タイは id 昇順で解決する。tie は highID より大きい id を持つので、
	// 同じ priority=20 でも highID（小さい方）が勝ち続ける。
	tie := insertRule(t, pool, ctx, "tie", 20)
	insertRuleKeyword(t, pool, ctx, tie, "対象番組")
	if tie <= highID {
		t.Fatalf("test assumption broken: tie id %d must be greater than highID %d", tie, highID)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (with tie): %v", err)
	}
	res2, ok := getReservation(t, pool, ctx, 4001)
	if !ok {
		t.Fatal("reservation missing after tie pass")
	}
	if res2.RuleID == nil || *res2.RuleID != highID {
		t.Errorf("tie-break winner = %v, want %d (smaller id)", res2.RuleID, highID)
	}
}

// 受け入れ基準 5: チャンネル識別 4 列（network_id/service_id/channel_type/channel）が
// EPG プロジェクションから埋まる。
func TestRunPass_FillsChannelColumns(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 5001, "チャンネルテスト", start)
	ruleID := insertRule(t, pool, ctx, "ch", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "チャンネルテスト")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 5001)
	if !ok {
		t.Fatal("reservation not created")
	}
	if res.NetworkID == nil || *res.NetworkID != 32736 {
		t.Errorf("network_id = %v, want 32736", res.NetworkID)
	}
	if res.ServiceID == nil || *res.ServiceID != 1024 {
		t.Errorf("service_id = %v, want 1024", res.ServiceID)
	}
	if res.ChannelType == nil || *res.ChannelType != "GR" {
		t.Errorf("channel_type = %v, want GR", res.ChannelType)
	}
	if res.Channel == nil || *res.Channel != "27" {
		t.Errorf("channel = %v, want 27", res.Channel)
	}
}

// 受け入れ基準 6: 番組情報のスナップショットは射影にある間は追従し、
// 射影から番組が消えると凍結される（前の値が残り、行も削除されない）。
func TestRunPass_SnapshotFollowsProjectionThenFreezes(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 6001, "延長番組", start)
	ruleID := insertRule(t, pool, ctx, "ext", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "延長番組")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	res1, ok := getReservation(t, pool, ctx, 6001)
	if !ok {
		t.Fatal("reservation not created")
	}
	if !res1.ProgramStartAt.Equal(start) {
		t.Fatalf("initial program_start_at = %v, want %v", res1.ProgramStartAt, start)
	}

	// 延長で EPG 側の開始時刻が繰り下がる → 予約行が追従する
	newStart := start.Add(10 * time.Minute)
	insertProgram(t, pool, ctx, 6001, "延長番組", newStart)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (after extension): %v", err)
	}
	res2, ok := getReservation(t, pool, ctx, 6001)
	if !ok {
		t.Fatal("reservation disappeared after extension")
	}
	if !res2.ProgramStartAt.Equal(newStart) {
		t.Errorf("snapshot did not follow projection update: got %v want %v", res2.ProgramStartAt, newStart)
	}

	// 射影から番組そのものが消える → 凍結（削除されず、前の値が残る）
	deleteProgram(t, pool, ctx, 6001)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (after projection removal): %v", err)
	}
	res3, ok := getReservation(t, pool, ctx, 6001)
	if !ok {
		t.Fatal("reservation should survive projection removal (frozen), not be deleted")
	}
	if !res3.ProgramStartAt.Equal(newStart) {
		t.Errorf("frozen program_start_at changed: got %v want %v", res3.ProgramStartAt, newStart)
	}
	if res3.Title != "延長番組" {
		t.Errorf("frozen title changed: got %q", res3.Title)
	}
}

// 受け入れ基準 7: ルールがマッチしなくなったとき、intent が無ければ削除、
// あれば残って rule_id が NULL になる（detached）。
func TestRunPass_RuleUnmatch_DeleteVsDetach(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 7001, "対象A", start)
	insertProgram(t, pool, ctx, 7002, "対象B", start)

	ruleA := insertRule(t, pool, ctx, "ruleA", 10)
	insertRuleKeyword(t, pool, ctx, ruleA, "対象A")
	ruleB := insertRule(t, pool, ctx, "ruleB", 10)
	insertRuleKeyword(t, pool, ctx, ruleB, "対象B")

	q := sqlcgen.New(pool)
	// 7002 には record intent が付いている（例: ユーザーが個別に上書き済み）
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 7002, Action: db.IntentRecord,
		ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	if _, ok := getReservation(t, pool, ctx, 7001); !ok {
		t.Fatal("7001 should be created initially")
	}
	if _, ok := getReservation(t, pool, ctx, 7002); !ok {
		t.Fatal("7002 should be created initially")
	}

	// ルールを無効化 → どちらも「マッチしなくなる」が、番組自体は射影に残っているので
	// 「ルールが本当にマッチしなくなった」と確信を持って判定できる。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id IN ($1, $2)`, ruleA, ruleB); err != nil {
		t.Fatal(err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after disabling rules: %v", err)
	}

	if _, ok := getReservation(t, pool, ctx, 7001); ok {
		t.Error("7001 (no intent) should be deleted once its rule stops matching")
	}
	res2, ok := getReservation(t, pool, ctx, 7002)
	if !ok {
		t.Fatal("7002 (has intent) should survive as detached, not be deleted")
	}
	if res2.RuleID != nil {
		t.Errorf("7002 rule_id = %v, want nil after detaching", res2.RuleID)
	}
	if res2.State != db.ReservationStateDetached {
		t.Errorf("7002 state = %q, want %q", res2.State, db.ReservationStateDetached)
	}
}

// 受け入れ基準 8: 1 パスの削除数が閾値を超える場合は削除せずサーキットブレーカーが
// 発火する。
func TestRunPass_CircuitBreakerBlocksBulkDelete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "bulk", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 5
	for i := range n {
		insertProgram(t, pool, ctx, int64(8000+i), fmt.Sprintf("対象%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	for i := range n {
		if _, ok := getReservation(t, pool, ctx, int64(8000+i)); !ok {
			t.Fatalf("program %d should have a reservation before disabling the rule", i)
		}
	}

	// ルール無効化 → n 件が一斉に unmatch。閾値 2 を超えるので削除は止まるはず。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after disabling rule: %v", err)
	}

	for i := range n {
		if _, ok := getReservation(t, pool, ctx, int64(8000+i)); !ok {
			t.Errorf("program %d should NOT be deleted while the circuit breaker is tripped", i)
		}
	}
}

// 受け入れ基準 9: reservation_rule_matches に、勝敗と無関係にマッチした全ルールが
// 記録される。
func TestRunPass_RecordsAllRuleMatches(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 9001, "複数マッチ", start)

	lowID := insertRule(t, pool, ctx, "low", 5)
	insertRuleKeyword(t, pool, ctx, lowID, "複数マッチ")
	highID := insertRule(t, pool, ctx, "high", 20)
	insertRuleKeyword(t, pool, ctx, highID, "複数マッチ")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 9001)
	if !ok {
		t.Fatal("reservation not created")
	}

	rows, err := pool.Query(ctx,
		`SELECT rule_id FROM reservation_rule_matches WHERE reservation_id = $1 ORDER BY rule_id`, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			t.Fatal(err)
		}
		got = append(got, rid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []int64{lowID, highID}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("reservation_rule_matches rule_ids = %v, want %v", got, want)
	}
}

// insertReservationDirect は reservations 行を ruler を介さず直接作る。
// GC のテストは「終了時刻 + 猶予」の境界だけを厳密に制御したいので、
// EPG プロジェクションやルールマッチに頼らず program_start_at/program_duration_ms
// を狙った値にできるこちらを使う。
func insertReservationDirect(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, title string, startAt time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
VALUES ($1, $2, 'manual', $3, $4, $5)`,
		testSite, programID, title, startAt, testDurationMs)
	if err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
}

func reservationExists(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64) bool {
	t.Helper()
	_, ok := getReservation(t, pool, ctx, programID)
	return ok
}

// 受け入れ基準 10（GC）: 番組終了 + RetentionGrace 経過の予約・program_intents・
// program_overrides は GC で削除される（同じ cutoff。docs/schema.md §3.5）。
func TestRunPass_GC_DeletesEndedPastGrace(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	grace := 1 * time.Hour
	// 終了時刻（start+duration）が now-1h30m。cutoff = now-grace(1h)。
	// 終了時刻 < cutoff なので GC 対象。
	start := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	insertReservationDirect(t, pool, ctx, 10001, "終了番組", start)

	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 10001, Action: db.IntentRecord,
		ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 10001, Overrides: []byte(`{"priority":3}`),
		ProgramStartAt: start, ProgramDurationMs: testDurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: grace})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if reservationExists(t, pool, ctx, 10001) {
		t.Error("reservation past end+grace should have been GC'd")
	}
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: testSite, ProgramID: 10001}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("program_intents past end+grace should have been GC'd, got err=%v", err)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: testSite, ProgramID: 10001}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("program_overrides past end+grace should have been GC'd, got err=%v", err)
	}
}

// 受け入れ基準 11（GC）: 終了しているが猶予（RetentionGrace）内の予約は残る。
func TestRunPass_GC_KeepsWithinGrace(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	grace := 1 * time.Hour
	// 終了時刻（start+duration）が now-10m。cutoff = now-1h。
	// 終了時刻(-10m) > cutoff(-1h) なので、終了済みだがまだ削除しない。
	start := time.Now().Add(-40 * time.Minute).Truncate(time.Second)
	insertReservationDirect(t, pool, ctx, 10002, "終了直後", start)

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: grace})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if !reservationExists(t, pool, ctx, 10002) {
		t.Error("reservation within the retention grace period should NOT be GC'd yet")
	}
}

// 受け入れ基準 12（GC）: 未終了の予約は残る。
func TestRunPass_GC_KeepsUnended(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertReservationDirect(t, pool, ctx, 10003, "未来番組", start)

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: 1 * time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if !reservationExists(t, pool, ctx, 10003) {
		t.Error("reservation for a program that hasn't ended yet should NOT be GC'd")
	}
}

// 受け入れ基準 13（GC）: GC は「ルール x EPG」からの導出削除を守るサーキット
// ブレーカー（MaxDeletesPerPass）とは独立しており、閾値を超える件数でも実行
// される。ここに置く予約はどれも EPG プロジェクションを持たないので、通常の
// 宣言的削除経路には（射影が無い間は凍結されるため）一切現れない —
// つまり削除が起きるとすれば GC 経由でしかありえない。
func TestRunPass_GC_IgnoresCircuitBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	grace := 1 * time.Hour
	start := time.Now().Add(-90 * time.Minute).Truncate(time.Second)

	const n = 5
	for i := range n {
		insertReservationDirect(t, pool, ctx, int64(11000+i), fmt.Sprintf("終了%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: grace, MaxDeletesPerPass: 1})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	for i := range n {
		if reservationExists(t, pool, ctx, int64(11000+i)) {
			t.Errorf("program %d should have been GC'd even though n=%d exceeds MaxDeletesPerPass=1", i, n)
		}
	}
}
