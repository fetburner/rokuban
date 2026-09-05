package ruler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/reservation"
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
  start_at, duration_ms, end_at, is_free, name, description
) VALUES ($1, $2, $3, $4, 0, $5, $6, $7, true, $8, '')
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

// reservationRow はテストで確認したい reservations / program_snapshots の列を
// 合わせ持つ。#27 で番組の事実のスナップショット（title / 開始時刻 / 尺 /
// チャンネル識別）が program_snapshots に抽出されたため JOIN して読む。
// State は #28/#30 で reservations から落ちた列で、(rule_id, base) から
// deriveTestState が毎回計算する（reservationFromRow と同じ式だが、同じ
// 実装を呼ばず独立に再実装してある — テストが検証対象と同じ実装を呼ぶと、
// その実装のバグをテストが見逃す）。orphaned は issue #98 で recordings の
// 存在から導出する値になったが、ruler は recordings に一切関与しない
// （書きも読みもしない）ので、ruler パッケージのテストは orphaned を
// 判定材料にしない（active/detached の 2 値だけで足りる）。
//
// source 列は issue #26 で削除済み（手動/ルール由来の区別は program_intents /
// rule_id から導出する。本ファイルにはこの列を前提にしたテストを置かない）。
type reservationRow struct {
	RuleID                *int64
	State                 string
	Base                  []byte
	Title                 string
	ProgramStartAt        time.Time
	ProgramDurationMs     int64
	NetworkID             *int32
	ServiceID             *int32
	ChannelType           *string
	Channel               *string
	DedupMatchRecordingID *int64
	DedupSimilarity       *float32
	UpdatedAt             time.Time
	CreatedAt             time.Time
}

// deriveTestState は internal/api.reservationState の active/detached 分岐
// （#28/#30 の決定）を独立に再実装したもの。rule_id が NULL かつ base が非
// NULL なら detached、それ以外は active。orphaned は含めない（上記コメント
// 参照 --- ruler のテストは recordings を作らないので、この関数が orphaned
// を返す状況が実際に起こらない）。
func deriveTestState(ruleID *int64, base []byte) string {
	switch {
	case ruleID == nil && len(base) > 0:
		return db.ReservationStateDetached
	default:
		return db.ReservationStateActive
	}
}

func getReservation(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64) (reservationRow, bool) {
	t.Helper()
	var r reservationRow
	err := pool.QueryRow(ctx, `
SELECT r.rule_id, r.base, s.title, s.start_at, s.duration_ms,
       s.network_id, s.service_id, s.channel_type, s.channel,
       r.dedup_match_recording_id, r.dedup_similarity, r.updated_at, r.created_at
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
WHERE r.site = $1 AND r.program_id = $2`, testSite, programID).Scan(
		&r.RuleID, &r.Base, &r.Title, &r.ProgramStartAt, &r.ProgramDurationMs,
		&r.NetworkID, &r.ServiceID, &r.ChannelType, &r.Channel,
		&r.DedupMatchRecordingID, &r.DedupSimilarity, &r.UpdatedAt, &r.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reservationRow{}, false
		}
		t.Fatalf("querying reservation for program %d: %v", programID, err)
	}
	r.State = deriveTestState(r.RuleID, r.Base)
	return r, true
}

func basePriority(t *testing.T, raw []byte) *int {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var opts reservation.Options
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

	// program_intents への FK（#27）を満たすため、ruler が動く前に
	// program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 1001, "ニュース7", start)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 1001, Action: reservation.IntentRecord,
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

	// program_overrides への FK（#27）を満たすため、ruler が動く前に
	// program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 1101, "上書きテスト", start)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 1101, Overrides: []byte(`{"priority":9}`),
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

	// program_overrides への FK（#27）を満たすため、ruler が動く前に
	// program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 1201, "上書き生存", start)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 1201, Overrides: []byte(`{"priority":9}`),
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

	// program_intents への FK（#27）を満たすため、ruler が動く前に
	// program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 2001, "映画スペシャル", start)
	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: testSite, ProgramID: 2001,
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

// 回帰テスト（#162）: desired の union を program_investments view 1 本の
// クエリに統合したリファクタが保つべき性質。intent{skip} と overrides が同じ
// 番組に同時にあっても、overrides の存在が skip 意図に優先して予約行を
// desired に残す（§4.3「意図または上書きがある → 削除せず detached で保持」）。
// program_intents.action='record' と 'skip' は同じ行を取り合うため record 側は
// 構造的に skip と排他だが、overrides は skip と独立に存在できるので、
// investment（record ∪ overrides）全体を skip で引いてはならない --- skip を
// 引くのは winner（ルールにマッチした番組）側だけでよい。もし誤って
// investment 全体から skip を引く実装に戻すと、この番組は desired から落ち、
// 予約行が消える。
func TestRunPass_SkipIntentWithOverrideSurvivesViaOverride(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 2011, "スキップと上書き併存", start)

	// この番組はどのルールにもマッチさせない（winner から意図的に外す）。
	// program_intents / program_overrides への FK（#27）を満たすため、
	// ruler が動く前に program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 2011, "スキップと上書き併存", start)
	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: testSite, ProgramID: 2011,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 2011, Overrides: []byte(`{"priority":9}`),
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	if _, ok := getReservation(t, pool, ctx, 2011); !ok {
		t.Error("reservation should survive via program_overrides even though intent{skip} is also set on the same program")
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

// issue #560: 予約が skip 意図だけに支えられる状態でも、射影に番組がある間は
// snapshot を追従させる。古い終了時刻のままだと GC の CASCADE で skip 意図まで
// 消えるため、番組を後ろ倒ししたときに意図が生き残ることまで確認する。
func TestRunPass_SkipIntentSnapshotFollowsDelayedProgramAndSurvivesGC(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	initialStart := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID int64 = 56001
	insertProgram(t, pool, ctx, programID, "後ろ倒し番組", initialStart)
	ruleID := insertRule(t, pool, ctx, "issue-560", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "後ろ倒し番組")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (initial): %v", err)
	}
	if !reservationExists(t, pool, ctx, programID) {
		t.Fatal("reservation should exist before the explicit skip")
	}

	// 同じ番組を EPG 側だけ後ろ倒しする。
	liveStart := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, programID, "後ろ倒し番組", liveStart)

	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: testSite, ProgramID: programID,
	}); err != nil {
		t.Fatalf("skipping program: %v", err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (after delayed skip): %v", err)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Fatal("skip-only program reservation should have been released before the GC regression pass")
	}

	// 予約が無くなった後に GC なら削除対象になる古い snapshot を作る。この時点では
	// skip 意図だけが snapshot 行を支えているので、修正前実装では次の RunPass で
	// snapshot が追従せず、GC の CASCADE で意図も消える。
	// 直前の RunPass（after delayed skip）では予約行がまだ existingSet に居たため、
	// 修正前実装でも snapshot は live EPG に追従してしまい、古い start_at のままには
	// ならない。そのためここで手で古い時刻へ戻して「skip 意図だけが支える凍結した
	// snapshot」を作り直す必要がある。この UPDATE を消すと、次の RunPass の時点で
	// snapshot がすでに live の start_at を持っており、回帰テストが修正前実装でも
	// 通ってしまう（＝何も検証しなくなる）。
	staleStart := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if _, err := pool.Exec(ctx, `
UPDATE program_snapshots SET start_at = $1
WHERE site = $2 AND program_id = $3`, staleStart, testSite, programID); err != nil {
		t.Fatalf("making stale snapshot fixture: %v", err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (skip-only GC regression): %v", err)
	}

	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{
		Site: testSite, ProgramID: programID,
	}); err != nil {
		t.Fatalf("skip intent should survive snapshot GC cascade: %v", err)
	}
	// snapshot は予約が消えた後も skip 意図の FK に支えられており、live EPG に
	// 追従しているので、古い終了時刻を条件にした GC の対象にならない。
	snapshot, err := q.GetProgramSnapshot(ctx, sqlcgen.GetProgramSnapshotParams{
		Site: testSite, ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("program snapshot should survive for skip intent: %v", err)
	}
	if !snapshot.StartAt.Equal(liveStart) {
		t.Errorf("program snapshot did not follow delayed EPG program: got %v want %v", snapshot.StartAt, liveStart)
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

	// program_intents への FK（#27）を満たすため、ruler が動く前に
	// program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 7002, "対象B", start)
	q := sqlcgen.New(pool)
	// 7002 には record intent が付いている（例: ユーザーが個別に上書き済み）
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 7002, Action: reservation.IntentRecord,
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

// 修正 1 の回帰テスト（方向 1/2）: DeleteReservationsBySiteAndProgramIDs は削除の
// 瞬間に program_intents.action='record' の存在を再評価し、呼び出し側が渡した
// programId が stale であっても実際には削除しない。
//
// runPassForSite は program_intents / program_overrides / 既存 reservations を
// トランザクション外で読んでから削除対象（toDelete）を計算し、実際の DELETE は
// 別のトランザクション内で後から実行される。api の CreateReservation
// （program_intents{record} と reservations 行を 1 tx でコミットする）が、この
// 読み取りと DELETE 実行の間に同じ program_id へ割り込むと、toDelete は古い
// 読み取りのままその番組を含んでしまい、作られたばかりの手動予約を削除して
// しまう。読み順を入れ替えても「計算してから DELETE を実行するまでの窓」は
// 必ず残るため、直すべきは DELETE 文自体のガードである。
//
// このテストは「stale な toDelete に基づいて DELETE が呼ばれた」状況を、実際の
// 予約行 + その後に着地した intent{record} という形で直接再現する
// （sqlcgen.DeleteReservationsBySiteAndProgramIDs は runPassForSite の実削除が
// 使うのと同じクエリなので、ここでの検証はそのまま runPassForSite の保護に
// 直結する）。
func TestRunPass_DeleteGuard_RecordIntentBlocksStaleDelete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 30001
	insertReservationDirect(t, pool, ctx, programID, "並行手動予約", start)

	q := sqlcgen.New(pool)
	// api の CreateReservation が、ruler の読み取りと DELETE 実行の間に割り込んで
	// 作った手動予約の意図。
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: programID, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}

	// runPassForSite がトランザクション外の古い読み取りに基づいて計算した、
	// 既に stale になった toDelete を模す。
	deleted, err := q.DeleteReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReservationsBySiteAndProgramIDsParams{
		Site:       testSite,
		ProgramIds: []int64{programID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("rows deleted = %d, want 0 (a concurrently-created record intent must block the stale delete)", deleted)
	}
	if !reservationExists(t, pool, ctx, programID) {
		t.Error("reservation with a concurrently-created record intent must survive the stale delete")
	}
}

// 修正 1 の回帰テスト（方向 2/2、罠の確認）: program_intents の EXISTS を
// action で絞らずに書くと、action='skip' の予約行（ユーザーが取消した予約）まで
// 保護されてしまい「取消した予約が消えない」という重大なリグレッションになる
// （ruler の desired 集合は skip を除外して行そのものを持たせない設計 =
// issue #18 の案 A）。ガードは action='record' に限定されているべきことを固定する。
func TestRunPass_DeleteGuard_SkipIntentDoesNotBlockDelete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 30002
	insertReservationDirect(t, pool, ctx, programID, "取消済み", start)

	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: testSite, ProgramID: programID,
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := q.DeleteReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReservationsBySiteAndProgramIDsParams{
		Site:       testSite,
		ProgramIds: []int64{programID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("rows deleted = %d, want 1 (a skip intent must NOT protect a stale reservation from deletion)", deleted)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Error("reservation with only a skip intent should have been deleted")
	}
}

// 受け入れ基準 8: 1 パスの削除数が閾値を超える場合は削除せずサーキットブレーカーが
// 発火する。M2-5 でこの発火は circuit_breakers への行の永続化になり（M1-4 はパス内
// 完結でここを検証できなかった）、detail には手動確認用に「何を消そうとしていたか」の
// 抜粋（programId + title）が入る。
//
// このテストは EPG プロジェクションが健全なまま（epg_programs は削除しない）ルールを
// 無効化する形で候補を作る。射影が生きているので stillProjectedSubset の凍結は働かず、
// ここで一斉削除を止めているのは大量削除サーキットブレーカーそのものである
// （EPGStation#692 のうち「ルールの一括編集/無効化で desired が大きく変わる」障害モード）。
// 射影自体が丸ごと消える障害モード（EPG 全欠損）は
// TestRunPass_FullEpgOutageFreezeStopsDeletesWithoutTrippingBreaker が別に固定する
// — 2 つは別の防御が別の障害モードを見ている。
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
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); ok {
		t.Fatal("circuit breaker should not be tripped before the bulk unmatch")
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

	cb, ok := getRulerDeletesBreaker(t, pool, ctx)
	if !ok {
		t.Fatal("circuit_breakers row should exist after tripping (the latch, docs/recording.md §3.2)")
	}
	if cb.Pending != n {
		t.Errorf("circuit_breakers.pending = %d, want %d", cb.Pending, n)
	}
	if cb.Threshold != 2 {
		t.Errorf("circuit_breakers.threshold = %d, want 2", cb.Threshold)
	}

	sample := decodeSample(t, cb.Detail)
	if sample.Total != n {
		t.Errorf("detail.total = %d, want %d", sample.Total, n)
	}
	if len(sample.Programs) == 0 {
		t.Fatal("detail.programs is empty — the manual-review sample is useless without it")
	}
	gotTitles := make(map[int64]string, len(sample.Programs))
	for _, p := range sample.Programs {
		gotTitles[p.ProgramID] = p.Title
	}
	// n=5 は breaker.MaxSampleSize (20) 未満なので、抜粋には全件が入っているはず。
	for i := range n {
		programID := int64(8000 + i)
		wantTitle := fmt.Sprintf("対象%d", i)
		title, ok := gotTitles[programID]
		if !ok {
			t.Errorf("detail.programs is missing programId %d", programID)
			continue
		}
		if title != wantTitle {
			t.Errorf("detail.programs[%d].title = %q, want %q", programID, title, wantTitle)
		}
	}
}

// ルールを削除ではなく無効化しても（ListEnabledRules から外れる）、
// program_overrides を持つ予約行は同じ複合キーのまま detached（rule_id = NULL）として
// 生き残ることの回帰テスト。
//
// program_overrides を先に足しておくことで、ルール無効化後も予約行自体は
// detached として生存させる。この経路は TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease
// （EPG 欠損による rule_id NULL 化）とは別で、ルール無効化によって winner から
// 一切マッチしなくなる経路をカバーするのはこのテストだけ。
func TestRunPass_DisablingRuleDetachesReservationWithInvestment(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 31001, "マッチ掃除", start)

	ruleID := insertRule(t, pool, ctx, "stale-match", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "マッチ掃除")

	// 予約行自体は detached として生存させるための投資（program_overrides）。
	// program_overrides への FK（#27）を満たすため、ruler が動く前に
	// program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 31001, "マッチ掃除", start)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 31001, Overrides: []byte(`{"priority":9}`),
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	res, ok := getReservation(t, pool, ctx, 31001)
	if !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}

	// ルールを削除ではなく無効化する。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after disabling rule: %v", err)
	}

	res2, ok := getReservation(t, pool, ctx, 31001)
	if !ok {
		t.Fatal("reservation with program_overrides should survive as detached, not be deleted")
	}
	if res2.RuleID != nil {
		t.Fatalf("rule_id = %v, want nil after detaching", res2.RuleID)
	}
	if !res2.CreatedAt.Equal(res.CreatedAt) {
		t.Fatalf("created_at changed from %v to %v: reservation row was recreated instead of detached in place",
			res.CreatedAt, res2.CreatedAt)
	}
}

// insertProgramSnapshotDirect は program_snapshots 行だけを直接作る。
// program_intents / program_overrides への FK（#27）を満たすためだけに使う
// 軽量ヘルパーで、reservations 行は作らない。api.CreateReservation が
// トランザクション内で「program_snapshots → program_intents/overrides →
// reservations」の順に書くのと同じ順序を、意図・上書きだけを単独で用意したい
// テストのために切り出したもの。
func insertProgramSnapshotDirect(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, title string, startAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ($1, $2, $3, $4, $5, 32736, 1024, 'GR', '27', 1, 'テスト局')
ON CONFLICT (site, program_id) DO UPDATE SET
  title = EXCLUDED.title, start_at = EXCLUDED.start_at, duration_ms = EXCLUDED.duration_ms`,
		testSite, programID, title, startAt, testDurationMs); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
}

// insertReservationDirect は program_snapshots + reservations 行を ruler を
// 介さず直接作る。GC のテストは「終了時刻 + 猶予」の境界だけを厳密に制御
// したいので、EPG プロジェクションやルールマッチに頼らず start_at/duration_ms
// を狙った値にできるこちらを使う。#27 で番組の事実のスナップショットが
// program_snapshots に抽出され、reservations への FK が張られたため、
// 予約行より先に program_snapshots を作る必要がある。
func insertReservationDirect(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, title string, startAt time.Time) {
	t.Helper()
	insertProgramSnapshotDirect(t, pool, ctx, programID, title, startAt)
	_, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id)
VALUES ($1, $2)`,
		testSite, programID)
	if err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
}

// insertNeverScheduledEvent は observed_at を狙って欠測観測を作る。通常の
// reconciler 経路では observed_at に DB の now() が入るため、GC の寿命境界を
// 固定したいテストだけがこのヘルパーを使う。
func insertNeverScheduledEvent(t *testing.T, pool *pgxpool.Pool, ctx context.Context, eventID int32, observedAt time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO never_scheduled_events (site, network_id, service_id, event_id, observed_at)
VALUES ($1, $2, $3, $4, $5)`, testSite, testNetworkID, testServiceID, eventID, observedAt)
	if err != nil {
		t.Fatalf("inserting never-scheduled event fixture: %v", err)
	}
}

func reservationExists(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64) bool {
	t.Helper()
	_, ok := getReservation(t, pool, ctx, programID)
	return ok
}

// getRulerDeletesBreaker は circuit_breakers から ruler_deletes ブレーカーの行を引く。
// 発動していなければ ok=false。
func getRulerDeletesBreaker(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (sqlcgen.CircuitBreaker, bool) {
	t.Helper()
	q := sqlcgen.New(pool)
	cb, err := q.GetCircuitBreaker(ctx, sqlcgen.GetCircuitBreakerParams{Site: testSite, Name: breaker.RulerDeletes})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.CircuitBreaker{}, false
		}
		t.Fatalf("querying circuit_breakers: %v", err)
	}
	return cb, true
}

// decodeSample は circuit_breakers.detail を breaker.Sample にデコードする。
func decodeSample(t *testing.T, detail []byte) breaker.Sample {
	t.Helper()
	var s breaker.Sample
	if err := json.Unmarshal(detail, &s); err != nil {
		t.Fatalf("unmarshalling circuit breaker detail %s: %v", detail, err)
	}
	return s
}

// deleteAllEpgPrograms は site の epg_programs を全削除する。mirakc 再起動・
// 再スキャン中の「EPG 全欠損」を模す（EPGStation#692 の障害クラス）。
func deleteAllEpgPrograms(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM epg_programs WHERE site = $1`, testSite); err != nil {
		t.Fatalf("deleting all epg_programs fixtures: %v", err)
	}
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
		Site: testSite, ProgramID: 10001, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: 10001, Overrides: []byte(`{"priority":3}`),
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

// issue #636: never_scheduled_events は program_snapshots の GC と別の寿命を持ち、
// retention_grace + 30 日を超えた観測だけが削除される。境界より新しい観測は
// event_id が再利用される可能性を考慮して残す。
func TestRunPass_GC_DeletesStaleNeverScheduledEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	grace := time.Hour
	insertNeverScheduledEvent(t, pool, ctx, 20001, time.Now().Add(-31*24*time.Hour))
	insertNeverScheduledEvent(t, pool, ctx, 20002, time.Now().Add(-29*24*time.Hour))

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: grace})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	for _, test := range []struct {
		eventID int32
		want    bool
	}{
		{eventID: 20001, want: false},
		{eventID: 20002, want: true},
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM never_scheduled_events
  WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4
)`, testSite, testNetworkID, testServiceID, test.eventID).Scan(&exists); err != nil {
			t.Fatalf("checking never-scheduled event %d: %v", test.eventID, err)
		}
		if exists != test.want {
			t.Errorf("never_scheduled_events event_id=%d exists=%v, want %v", test.eventID, exists, test.want)
		}
	}
}

// issue #636: 古い欠測観測を GC した後は、同じ放送イベントの予約が
// ListReservationsForSyncEvaluation の候補に戻る。program_snapshots は未来の
// 番組なので、ここでは snapshot 自体の GC ではなく欠測行の GC だけが効く。
func TestRunPass_GC_AllowsStaleNeverScheduledReservationBackIntoSyncEvaluation(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	const programID = 20003
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertReservationDirect(t, pool, ctx, programID, "欠測 GC 後に同期候補へ戻る番組", start)
	insertNeverScheduledEvent(t, pool, ctx, 1, time.Now().Add(-31*24*time.Hour))

	q := sqlcgen.New(pool)
	before, err := q.ListReservationsForSyncEvaluation(ctx, testSite)
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation before GC: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("reservation should be excluded while its never-scheduled event exists, got %d candidates", len(before))
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetentionGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	after, err := q.ListReservationsForSyncEvaluation(ctx, testSite)
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation after GC: %v", err)
	}
	if len(after) != 1 || after[0].ProgramSnapshot.ProgramID != programID {
		t.Fatalf("after GC candidates = %#v, want only program_id=%d", after, programID)
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

// 受け入れ基準（M2-5 ラッチ 1/2）: 発動中は、次のパスで削除候補が閾値以下に
// 戻っても削除されない。M1-4 の骨格はパス内で完結していたのでこの区別が
// できなかった — ここが M2-5 のラッチ化の核心（自動では解除しない）。
func TestRunPass_CircuitBreakerLatchBlocksDeleteEvenBelowThreshold(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "latch", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(20000+i), fmt.Sprintf("対象%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}

	// ルール無効化 → 3 件が unmatch。閾値 2 を超えるので発動する。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Fatal("circuit breaker should be tripped")
	}

	// 20000 番組にだけ record intent を付ける → 実際の削除候補が 2 件（閾値 2 以下）に
	// 減る。20000 自身は intent により desired に残るので、そもそも削除候補ではない。
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 20000, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (still latched, candidates now below threshold): %v", err)
	}

	// 20001/20002 は実際の削除候補（2 件 <= 閾値 2）だが、ラッチが解けていないので
	// 削除されない。
	for i := 1; i < n; i++ {
		programID := int64(20000 + i)
		if !reservationExists(t, pool, ctx, programID) {
			t.Errorf("program %d should NOT be deleted — the circuit breaker is latched even though pending (2) <= threshold (2)", programID)
		}
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Error("circuit breaker should still be tripped (a latch does not auto-clear when candidates drop)")
	}
}

// issue #556: サーキットブレーカーのラッチで削除を見送られている（= desired
// ではないが existingSet にはまだ居る）行も、猶予と同じ経路で program_snapshots
// が追従することを確認する。罠（issue #556 本文）: 猶予だけを特別扱いする
// 直し方をすると、ラッチで残った行のスナップショットは凍結されたままになる。
func TestRunPass_CircuitBreakerLatch_SnapshotFollowsLiveWhileWithheld(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "latch-snapshot-follow", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(23000+i), fmt.Sprintf("対象%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}

	// ルール無効化 → 3 件が unmatch。閾値 2 を超えるので発動する。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Fatal("circuit breaker should be tripped")
	}
	const programID = 23001
	if !reservationExists(t, pool, ctx, programID) {
		t.Fatalf("program %d should be withheld right after tripping", programID)
	}

	// ラッチで削除を見送られている間に、23001 の EPG 開始時刻を繰り上げる。
	// この行は desired ではない（ルールは無効なまま）が、まだ reservations に
	// 居るので program_snapshots は追従するはず。
	liveStart := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	insertProgram(t, pool, ctx, programID, "対象1", liveStart)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (while latched): %v", err)
	}

	res, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatalf("program %d should still be withheld while latched (not deleted)", programID)
	}
	if !res.ProgramStartAt.Equal(liveStart) {
		t.Errorf("program_snapshots.start_at = %v, want %v (must follow the live EPG value even while withheld by the latch, not just while desired)", res.ProgramStartAt, liveStart)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Error("circuit breaker should still be tripped")
	}
}

// 受け入れ基準（M2-5 ラッチ 2/2）: 発動中でも削除以外は止まらない。新規予約の
// 作成・既存予約のスナップショット追従（延長等）は続く（止めるのは導出削除だけ。
// docs/recording.md §3.2「発動はラッチ」）。
func TestRunPass_TrippedCircuitBreakerStillCreatesAndUpdatesReservations(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	bulkRule := insertRule(t, pool, ctx, "bulk-trigger", 10)
	insertRuleKeyword(t, pool, ctx, bulkRule, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(21000+i), fmt.Sprintf("対象%d", i), start)
	}

	// 21000 は record intent で常に desired に残す。ブレーカー発動後にこの番組の
	// スナップショット追従（延長）が続くことを確認するため。program_intents への
	// FK（#27）を満たすため、ruler が動く前に program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, 21000, "対象0", start)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 21000, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 1})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}

	// ルール無効化 → 21001/21002 が unmatch（intent の無い 2 件 > 閾値 1）で発動。
	// 21000 は intent により desired のまま残る。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, bulkRule); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Fatal("circuit breaker should be tripped")
	}
	for i := 1; i < n; i++ {
		if !reservationExists(t, pool, ctx, int64(21000+i)) {
			t.Fatalf("program %d should be withheld right after tripping", 21000+i)
		}
	}

	// 発動中に 21000 の EPG 開始時刻を延長する（更新は止まらないはず）。
	newStart := start.Add(10 * time.Minute)
	insertProgram(t, pool, ctx, 21000, "対象0", newStart)

	// 発動中に新しい番組・新しいルールを追加する（作成は止まらないはず）。
	newRule := insertRule(t, pool, ctx, "new-while-tripped", 10)
	insertRuleKeyword(t, pool, ctx, newRule, "新規番組")
	insertProgram(t, pool, ctx, 21100, "新規番組", newStart)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (while latched): %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 21000)
	if !ok {
		t.Fatal("program 21000 (has record intent) should survive")
	}
	if !res.ProgramStartAt.Equal(newStart) {
		t.Errorf("program 21000 snapshot did not follow the EPG update while the breaker is latched: got %v want %v", res.ProgramStartAt, newStart)
	}
	if _, ok := getReservation(t, pool, ctx, 21100); !ok {
		t.Error("a brand-new reservation should still be created while the circuit breaker is latched")
	}

	// ブレーカーは相変わらず発動中で、21001/21002 は削除されない。
	for i := 1; i < n; i++ {
		if !reservationExists(t, pool, ctx, int64(21000+i)) {
			t.Errorf("program %d should still be withheld while latched", 21000+i)
		}
	}
}

// 受け入れ基準（M2-5 ラッチ + GC）: ブレーカーが発動中でも、番組終了後の GC
// （runGC）は独立して動き続ける。GC の削除対象は時刻の比較だけで決定的に定まり
// EPG の状態に左右されないので、ここまで止めると実害のない削除が積み上がる
// だけになる（docs/recording.md §3.2「番組終了後の GC」）。
//
// TestRunPass_GC_IgnoresCircuitBreaker は「同じパス内」で候補数が閾値を超えても
// GC が動くことを固定しているのに対し、このテストは「別パスで既に発動済みの
// ラッチが持ち越された状態」でも GC が動くことを固定する。
func TestRunPass_GC_RunsWhileCircuitBreakerLatched(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "gc-latch", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(22000+i), fmt.Sprintf("対象%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 1, RetentionGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}

	// 一斉 unmatch でブレーカーを発動させる（GC とは無関係に発生させる）。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Fatal("circuit breaker should be tripped")
	}

	// 発動中に GC 対象（終了 + 猶予経過）の予約を直接投入する。
	endedStart := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	insertReservationDirect(t, pool, ctx, 22900, "終了済み", endedStart)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (latched, with a GC-eligible row): %v", err)
	}

	if reservationExists(t, pool, ctx, 22900) {
		t.Error("GC-eligible reservation should be deleted even while the circuit breaker is latched")
	}
	// ラッチ対象の予約自体はまだ人間の確認待ちのまま残る。
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(22000+i)) {
			t.Errorf("program %d should still be withheld by the latched circuit breaker", 22000+i)
		}
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Error("circuit breaker should still be latched (GC does not touch it)")
	}
}

// 受け入れ基準（M2-5 手動再開で収束）: ResumeCircuitBreaker で行を消すと、次の
// パスで削除候補が閾値以下であれば実際に削除が実行され、収束する
// （M2-5 の受け入れ条件「手動再開で収束すること」）。
func TestRunPass_ResumeCircuitBreakerConverges(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "resume", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(23000+i), fmt.Sprintf("対象%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}

	// 一斉 unmatch → 3 件 > 閾値 2 で発動。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(23000+i)) {
			t.Fatalf("program %d should survive the initial trip", 23000+i)
		}
	}

	// 手動確認の結果、1 件（23000）だけ record intent を足して残す運用判断をしたと
	// 仮定する（実運用では detail を見て個別に intent/override を足す、または
	// 閾値を上げる）。残り 2 件は依然として削除候補だが、これで候補数が
	// 閾値（2）以下に収まる。
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 23000, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}

	// 再開前: ラッチはまだ効いているので、候補が減っても削除は起きない。
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (still latched): %v", err)
	}
	for i := 1; i < n; i++ {
		if !reservationExists(t, pool, ctx, int64(23000+i)) {
			t.Fatalf("program %d should still be withheld before resume", 23000+i)
		}
	}

	// 手動再開。
	if _, err := q.ResumeCircuitBreaker(ctx, sqlcgen.ResumeCircuitBreakerParams{Site: testSite, Name: breaker.RulerDeletes}); err != nil {
		t.Fatalf("ResumeCircuitBreaker: %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); ok {
		t.Fatal("circuit_breakers row should be gone after resume")
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (after resume): %v", err)
	}

	// 収束: 23000 は intent により生存、23001/23002 は削除される。
	if !reservationExists(t, pool, ctx, 23000) {
		t.Error("program 23000 (has a record intent) should survive")
	}
	for i := 1; i < n; i++ {
		if reservationExists(t, pool, ctx, int64(23000+i)) {
			t.Errorf("program %d should be deleted after resume converges", 23000+i)
		}
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); ok {
		t.Error("circuit breaker should not re-trip: pending candidates (2) are at the threshold (2), not above it")
	}
}

// 受け入れ基準 7（M2-5 ラッチ）: 発動中に候補数が閾値を超え続けても、tripped_at は
// 最初の発動時刻のまま更新されない（「いつから止まっているか」が運用上の関心事。
// TripCircuitBreaker の ON CONFLICT。docs/recording.md §3.2）。
func TestRunPass_CircuitBreakerTrippedAtNotUpdatedOnRepeatedTrip(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "repeat-trip", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(24000+i), fmt.Sprintf("対象%d", i), start)
	}

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 1})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (first trip): %v", err)
	}
	first, ok := getRulerDeletesBreaker(t, pool, ctx)
	if !ok {
		t.Fatal("circuit breaker should be tripped")
	}

	// 候補は 3 件のまま（依然として閾値 1 を超え続ける）なので、次のパスでも
	// Trip が呼ばれ直す。pending/detail は更新されてよいが、tripped_at は
	// 据え置かれるはず。
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (second trip, still over threshold): %v", err)
	}
	second, ok := getRulerDeletesBreaker(t, pool, ctx)
	if !ok {
		t.Fatal("circuit breaker should still be tripped")
	}

	if !first.TrippedAt.Equal(second.TrippedAt) {
		t.Errorf("tripped_at changed on a repeated trip: %v -> %v", first.TrippedAt, second.TrippedAt)
	}
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(24000+i)) {
			t.Errorf("program %d should still be withheld", 24000+i)
		}
	}
}

// 受け入れ基準（M2-5、EPGStation#692 の障害クラス）: 射影（epg_programs）が
// 丸ごと消えると、ルールがマッチしなくなった番組は「凍結」
// （stillProjectedSubset、docs/schema.md「射影にある間は更新、消えたら凍結」）されて
// 削除候補にすらならない。これは大量削除サーキットブレーカーとは**別の防御**であり、
// ブレーカーは一度も発動しない（凍結によって toDelete が空になるため）。
//
// TestRunPass_CircuitBreakerBlocksBulkDelete は射影が健全なままルールが無効化される
// 別の障害モードを固定しており、そちらではブレーカーそのものが一斉削除を止めている。
// 2 つのテストは別々の防御が別々の障害モードを見ていることを示す対になっている。
func TestRunPass_FullEpgOutageFreezeStopsDeletesWithoutTrippingBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "epg-outage", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 5
	for i := range n {
		insertProgram(t, pool, ctx, int64(25000+i), fmt.Sprintf("対象%d", i), start)
	}

	// 閾値をわざと小さくしておく: 理屈上はブレーカーが発動しうる状況でも、
	// 凍結が先に効いて toDelete が空になるので発動しないことを示すため。
	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 1})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(25000+i)) {
			t.Fatalf("program %d should have a reservation before the EPG outage", 25000+i)
		}
	}

	// mirakc 再起動・再スキャン等による EPG 全欠損を模す。
	deleteAllEpgPrograms(t, pool, ctx)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (EPG outage): %v", err)
	}

	for i := range n {
		if !reservationExists(t, pool, ctx, int64(25000+i)) {
			t.Errorf("program %d should survive as frozen, not deleted, during the EPG outage", 25000+i)
		}
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); ok {
		t.Error("circuit breaker should NOT trip — the freeze (stillProjectedSubset) already stopped the deletes before the breaker's threshold check")
	}
}

// TestRunPass_ManualReservationSurvivesRuleMatchWithoutSourceColumn は issue #26
// の受け入れ基準 5: ruler の全量パスを回しても、手動予約が「ルール由来」に
// 化けないことを固定する。
//
// reservations.source 列は issue #26 で削除済みなので、
// 修正前のバグ（手動予約にルールがマッチすると source が不可逆に 'rule' へ
// 書き換わる）は列ごと構造的に起こりようがない。このテストは 2 つを固定する:
//
//   - (a) reservations テーブルに source 列が存在しないこと。将来「ruler が
//     rule_id を書くたびに provenance 的な列も更新しておけば便利では」という
//     形で同じ歪みが紛れ込む変更への防波堤にする。
//   - (b) 手動予約（program_intents{record}）にルールがマッチして rule_id が
//     実際に埋まった後も、program_intents.action は RunPass を経て 'record' の
//     ままであること。「手動である」という事実の唯一の記録先が生き続けている
//     ことを確認する（TestRunPass_DoesNotWriteProgramIntents と似ているが、
//     こちらは「ルールが実際にマッチした」状態で確認する点が違う）。
func TestRunPass_ManualReservationSurvivesRuleMatchWithoutSourceColumn(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 1301
	insertProgram(t, pool, ctx, programID, "手動予約サバイバル", start)

	// 手動予約: 予約行 + intent{record}。ruler が動く前は rule_id はまだ付いていない。
	// program_intents への FK があるため、予約行（と、それが作る program_snapshots
	// 行）を先に作ってから intent を書く（api.CreateReservation と同じ順序）。
	insertReservationDirect(t, pool, ctx, programID, "手動予約サバイバル", start)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: programID, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}

	// 後からマッチするルールを用意する（修正前バグの引き金と同じ状況）。
	ruleID := insertRule(t, pool, ctx, "manual-survivor", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "手動予約サバイバル")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	// ルールが実際にマッチして rule_id が埋まったことを確認する
	// （これが起きていなければこのテスト自体が無意味なので前提として確認する）。
	res, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should still exist")
	}
	if res.RuleID == nil || *res.RuleID != ruleID {
		t.Fatalf("rule_id = %v, want %d (rule should have matched)", res.RuleID, ruleID)
	}

	// 核心 (b): program_intents.action は ruler を経ても変わらない。
	intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: testSite, ProgramID: programID})
	if err != nil {
		t.Fatalf("program_intents row must survive: %v", err)
	}
	if intent.Action != reservation.IntentRecord {
		t.Errorf("program_intents.action = %q, want %q "+
			"(手動である事実はルールマッチでも消えないはず。issue #26)", intent.Action, reservation.IntentRecord)
	}

	// 核心 (a): source 列は存在しない（将来の焼き直しへの防波堤）。
	var exists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'reservations' AND column_name = 'source'
)`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("reservations.source column must not exist (issue #26 removed it; " +
			"do not reintroduce a derived source-like column on reservations)")
	}
}

// insertManualReservation は「手動予約」（ルールがマッチせず intent{record} だけで
// desired になる番組）を EPG プロジェクション込みで用意する。ruler が実体化した
// 予約行は rule_id が NULL になり、intent をクリアすると「ユーザーが投資を消さない
// 限り起きない削除」の対象になる。削除候補になったときに stillProjectedSubset で
// 凍結されないよう、epg_programs にも行を入れる（どのルールにもマッチしない
// 題名を使う）。
func insertManualReservation(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, title string, startAt time.Time) {
	t.Helper()
	insertProgram(t, pool, ctx, programID, title, startAt)
	insertProgramSnapshotDirect(t, pool, ctx, programID, title, startAt)
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: programID, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatalf("upserting record intent for program %d: %v", programID, err)
	}
}

// issue #171 の核心: ラッチ中でも intent クリア（DELETE .../intent）由来の削除は
// 保留されない。
//
// 修正前は toDelete が「desired から外れた理由」を区別しなかったため、intent を
// クリアしても予約行が existing のまま残り、effective.skip も立たない（クリアは
// 「意見なし」であって「録るな」ではない）ので listDesired からも除外されず、
// 人間が resume するまで番組が録画され続けた。ブレーカーが守るのは「ルール x EPG」
// 由来の削除であり、intent クリアはユーザーが投資を消す書き込みをしない限り起きない
// （その根拠は NOT EXISTS program_investments のほうにある ---
// TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease と
// internal/db/queries/ruler.sql のコメント参照。rule_id IS NULL は EPG の変化
// だけでも立つのでユーザー由来の証明にはならない）。
//
// 反対方向（ルール由来の削除はラッチ中に保留され続ける）は
// TestRunPass_CircuitBreakerLatchBlocksDeleteEvenBelowThreshold と、
// このテスト内の ruleProgram の確認が固定する。
func TestRunPass_LatchDoesNotWithholdIntentClearDelete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "latch-trigger", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(31000+i), fmt.Sprintf("対象%d", i), start)
	}
	// ルールにマッチしない手動予約。intent{record} だけで desired になる。
	const manualProgramID = 31900
	insertManualReservation(t, pool, ctx, manualProgramID, "手動で録りたい番組", start)

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	manual, ok := getReservation(t, pool, ctx, manualProgramID)
	if !ok {
		t.Fatal("manual reservation should exist after the first pass")
	}
	if manual.RuleID != nil {
		t.Fatalf("manual reservation rule_id = %v, want nil (no rule should match it)", manual.RuleID)
	}

	// ルール無効化 → 3 件が unmatch（> 閾値 2）でブレーカー発動。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Fatal("circuit breaker should be tripped")
	}

	// ラッチ中にユーザーが手動予約の意図をクリアする（「意見なし」に戻す）。
	q := sqlcgen.New(pool)
	if _, err := q.DeleteProgramIntent(ctx, sqlcgen.DeleteProgramIntentParams{
		Site: testSite, ProgramID: manualProgramID,
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (while latched): %v", err)
	}

	if reservationExists(t, pool, ctx, manualProgramID) {
		t.Error("intent クリア後の予約はラッチ中でも削除されるべき " +
			"（残すと effective.skip も立たないため番組が録画され続ける。issue #171）")
	}
	// 反対方向: ルール由来の削除は依然として保留されている。
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(31000+i)) {
			t.Errorf("program %d (rule-derived unmatch) must still be withheld while latched", 31000+i)
		}
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Error("circuit breaker should still be latched (an explicit delete must not resume it)")
	}
}

// issue #171: 明示操作由来の削除はブレーカーの**数にも入らない**。intent を
// まとめてクリアしても、その件数だけでブレーカーが発動してはいけない
// （EPG の欠損・フリッカーではこの集合を作れないので、ここを数えても
// EPGStation#692 クラスの防御にはならず、ユーザー操作を人質に取るだけになる）。
func TestRunPass_IntentClearDeletesDoNotCountTowardBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	const n = 3
	for i := range n {
		insertManualReservation(t, pool, ctx, int64(32000+i), fmt.Sprintf("手動%d", i), start)
	}

	// 閾値 2 < 3 件。混同していれば発動する。
	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(32000+i)) {
			t.Fatalf("manual reservation %d should exist after the first pass", 32000+i)
		}
	}

	q := sqlcgen.New(pool)
	for i := range n {
		if _, err := q.DeleteProgramIntent(ctx, sqlcgen.DeleteProgramIntentParams{
			Site: testSite, ProgramID: int64(32000 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after clearing intents: %v", err)
	}

	for i := range n {
		if reservationExists(t, pool, ctx, int64(32000+i)) {
			t.Errorf("manual reservation %d should be deleted after its intent was cleared", 32000+i)
		}
	}
	if cb, ok := getRulerDeletesBreaker(t, pool, ctx); ok {
		t.Errorf("circuit breaker must not trip on explicit deletes (pending = %d, threshold = %d)",
			cb.Pending, cb.Threshold)
	}
}

// issue #171: intent skip 由来の削除もラッチ中に保留されない。skip の実害は
// 「予約一覧に消えない行が残る」だけ（effective.skip が録画自体は防ぐ）だが、
// 明示操作をブレーカーの外に出す線は skip とクリアで分けない — 分けると
// 「明示操作は対象外、ただし skip を除く」という覚えておくべき例外が増える。
func TestRunPass_LatchDoesNotWithholdSkipIntentDelete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	// ブレーカーを発動させるためのルール（無効化して一斉 unmatch させる）。
	triggerRule := insertRule(t, pool, ctx, "latch-trigger", 10)
	insertRuleKeyword(t, pool, ctx, triggerRule, "臨時")
	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(33000+i), fmt.Sprintf("臨時%d", i), start)
	}

	// こちらは有効なまま残るルール。ユーザーはこのルールが作った予約を取消す。
	keepRule := insertRule(t, pool, ctx, "keeper", 10)
	insertRuleKeyword(t, pool, ctx, keepRule, "定期")
	const skippedProgramID = 33900
	insertProgram(t, pool, ctx, skippedProgramID, "定期番組", start)

	r := ruler.New([]string{testSite}, pool, &ruler.Config{MaxDeletesPerPass: 2})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	kept, ok := getReservation(t, pool, ctx, skippedProgramID)
	if !ok {
		t.Fatal("the rule-backed reservation should exist after the first pass")
	}
	if kept.RuleID == nil {
		t.Fatal("the rule-backed reservation should have a rule_id (otherwise this test would not exercise the skip branch)")
	}

	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, triggerRule); err != nil {
		t.Fatal(err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (trip): %v", err)
	}
	if _, ok := getRulerDeletesBreaker(t, pool, ctx); !ok {
		t.Fatal("circuit breaker should be tripped")
	}

	// ラッチ中にユーザーが「録るな」を書く。
	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: testSite, ProgramID: skippedProgramID,
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (while latched): %v", err)
	}

	if reservationExists(t, pool, ctx, skippedProgramID) {
		t.Error("skip intent の予約はラッチ中でも削除されるべき（明示操作はブレーカーの外。issue #171）")
	}
	for i := range n {
		if !reservationExists(t, pool, ctx, int64(33000+i)) {
			t.Errorf("program %d (rule-derived unmatch) must still be withheld while latched", 33000+i)
		}
	}
}

// 罠（#29 型の窓）の回帰テスト（方向 1/2）: 明示操作由来の DELETE も、削除の瞬間に
// program_investments を再評価する。runPassForSite が toDelete を計算してから
// この文を実行するまでの間に record 意図が着地したら、削除してはならない。
//
// DeleteReleasedReservationsBySiteAndProgramIDs は runPassForSite の実削除が使うのと
// 同じクエリなので、ここでの検証はそのまま runPassForSite の保護に直結する
// （TestRunPass_DeleteGuard_RecordIntentBlocksStaleDelete と同じ組み立て）。
func TestRunPass_ReleasedDeleteGuard_RecordIntentBlocksStaleDelete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 34001
	// rule_id が NULL の予約行 = 明示操作由来の削除の候補になりうる形。
	insertReservationDirect(t, pool, ctx, programID, "並行手動予約", start)

	q := sqlcgen.New(pool)
	// ruler の読み取りと DELETE 実行の間に割り込んで着地した record 意図。
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: programID, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatal(err)
	}

	released, err := q.DeleteReleasedReservationsBySiteAndProgramIDs(ctx,
		sqlcgen.DeleteReleasedReservationsBySiteAndProgramIDsParams{
			Site:       testSite,
			ProgramIds: []int64{programID},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want empty (a concurrently-created record intent must block the stale delete)", released)
	}
	if !reservationExists(t, pool, ctx, programID) {
		t.Error("reservation with a concurrently-created record intent must survive the stale delete")
	}
}

// 実測で固定する事実（PR #273 のレビューで最初の論証が偽と判明した箇所）:
// **`reservations.rule_id` は EPG の変化だけでも NULL になる。** 投資
// （`program_overrides` / `intent{record}`）を持つ行はルールが外れても desired に
// 残るのでそのパスで upsert され、`internal/ruler/sql.go` の resolved CTE が凍結
// するのは `base` と dedup 根拠 2 列だけで、`rule_id = EXCLUDED.rule_id` が
// そのまま NULL を書くため。
//
// したがって `rule_id IS NULL` は「ユーザー由来の削除」の証明にならない。
// 明示操作由来の削除をブレーカーの外に出せる根拠は `NOT EXISTS
// program_investments` のほうにあり、このテストは 3 点を測る:
//
//  1. EPG だけが動いた（ルールがマッチしなくなった）投資つきの行は rule_id が NULL になる
//  2. **投資がある限り released にはならない**（守備範囲を保っているのはこの条件）
//  3. 投資を消すと released になる = 境界 (c)（docs/recording/breaker.md）。
//     EPG 欠損中に投資を消すと、健全な EPG ならルール由来で残ったはずの予約が
//     ブレーカーの外で消える。次パスでルールが作り直すので自己修復する
//
// TestRunPass_ReleasedDeleteGuard_RecordIntentBlocksStaleDelete は「あとから
// record 意図が着地する」形なので、この「EPG が rule_id を落とした行」の形は
// 押さえていない。
func TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 36001
	insertProgram(t, pool, ctx, programID, "対象番組", start)
	ruleID := insertRule(t, pool, ctx, "epg-nulls-rule-id", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	res, ok := getReservation(t, pool, ctx, programID)
	if !ok || res.RuleID == nil {
		t.Fatalf("reservation should exist with a rule_id after the first pass (exists=%v rule_id=%v)", ok, res.RuleID)
	}

	// ユーザーが上書きを 1 つ置く（investment）。これでルールが外れても desired に残る。
	q := sqlcgen.New(pool)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: programID, Overrides: []byte(`{"priority":3}`),
	}); err != nil {
		t.Fatal(err)
	}

	// **EPG だけが動く**（番組名が変わってルールがマッチしなくなる）。ユーザーは何もしていない。
	insertProgram(t, pool, ctx, programID, "別の番組", start)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after the EPG-only change: %v", err)
	}

	// 1. rule_id は EPG の変化だけで NULL になる。
	res, ok = getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should survive via the investment")
	}
	if res.RuleID != nil {
		t.Fatalf("rule_id = %d, want nil "+
			"（EPG の変化だけで NULL になる。この事実の上に released の論証が乗っている）", *res.RuleID)
	}

	// 2. 投資がある限り released にならない。ここが守備範囲を保っている条件。
	released, err := q.DeleteReleasedReservationsBySiteAndProgramIDs(ctx,
		sqlcgen.DeleteReleasedReservationsBySiteAndProgramIDsParams{
			Site: testSite, ProgramIds: []int64{programID},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want empty "+
			"（EPG が rule_id を落としただけの行がブレーカーの外で消えてはならない）", released)
	}
	if !reservationExists(t, pool, ctx, programID) {
		t.Fatal("reservation must survive while the investment exists")
	}

	// 3. 投資を消すと released になる（境界 (c)。ユーザーの書き込みが要ることの裏返し）。
	if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
		Site: testSite, ProgramID: programID,
	}); err != nil {
		t.Fatal(err)
	}
	released, err = q.DeleteReleasedReservationsBySiteAndProgramIDs(ctx,
		sqlcgen.DeleteReleasedReservationsBySiteAndProgramIDsParams{
			Site: testSite, ProgramIds: []int64{programID},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0] != programID {
		t.Errorf("released = %v, want [%d] (removing the last investment must release the row)", released, programID)
	}
}

// 罠の回帰テスト（方向 2/2）: ルールが base を供給していた予約（rule_id 非 NULL、
// skip 意図なし）は明示操作由来の DELETE の対象にならない。ここが漏れると
// 「ルールが EPG の欠損でマッチしなくなった」削除がブレーカーを迂回し、
// EPGStation#692 クラスの一斉削除に対する防御そのものが消える。
func TestRunPass_ReleasedDeleteGuard_RuleBackedRowIsNotReleased(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 35001
	insertProgram(t, pool, ctx, programID, "ルール由来", start)
	ruleID := insertRule(t, pool, ctx, "rule-backed", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "ルール由来")

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	res, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should exist after the pass")
	}
	if res.RuleID == nil {
		t.Fatal("reservation should carry a rule_id (otherwise this test asserts nothing)")
	}

	// EPG の欠損でルールがマッチしなくなった状況を模して、削除候補として渡す。
	q := sqlcgen.New(pool)
	released, err := q.DeleteReleasedReservationsBySiteAndProgramIDs(ctx,
		sqlcgen.DeleteReleasedReservationsBySiteAndProgramIDsParams{
			Site:       testSite,
			ProgramIds: []int64{programID},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want empty (a rule-backed unmatch must stay subject to the circuit breaker)", released)
	}
	if !reservationExists(t, pool, ctx, programID) {
		t.Error("a rule-backed reservation must not be deleted by the explicit-operation delete")
	}
}

// 猶予（ruler.retract_grace, issue #428）のテスト群。
//
// 番組表は放送直前まで書き換わる（「[新]」が付く、サブタイトルが入る、誤字が直る）。
// その拍子にルールの条件から外れた予約は、猶予が無いと次のパスで desired から
// 落ちて削除される --- 開始 30 分前に題名が 1 文字直っただけで録り逃す経路が開く。
// 猶予はこの経路を、放送開始が近い予約に限って塞ぐ（denpa の RULE_RETRACT_GRACE に
// 倣う）。
//
// retractGraceUnmatch はルールにマッチしていた番組のタイトルを書き換えて、
// 「ルールから外れた」状況（EPG は生きているが条件を満たさなくなった）を作る。
// ルールの無効化（TestRunPass_RetractGrace_DisabledRuleIsNotProtected が別に見る）
// とは異なる経路であることに注意 --- こちらは番組自体は射影に残ったままなので
// stillProjectedSubset の凍結は働かず、猶予の述語だけが効く。
func retractGraceUnmatch(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, newTitle string, start time.Time) {
	t.Helper()
	insertProgram(t, pool, ctx, programID, newTitle, start)
}

// (a) 開始 30 分前に条件から外れた予約は、猶予が有効なら次パスでも残る。
// 罠の確認: 猶予の述語（ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs の
// WHERE 句）を丸ごと外す（`AND false` を足す）とこのテストは落ちる。
func TestRunPass_RetractGrace_KeepsImminentUnmatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	const programID = 40001
	insertProgram(t, pool, ctx, programID, "対象番組", start)

	ruleID := insertRule(t, pool, ctx, "grace-imminent", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	before, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}
	if before.RuleID == nil {
		t.Fatal("reservation should carry a rule_id (otherwise this test asserts nothing)")
	}

	// 番組表の書き換えでルールから外れる（タイトルからキーワードが消える）。
	retractGraceUnmatch(t, pool, ctx, programID, "非マッチ番組", start)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after unmatch: %v", err)
	}

	after, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should survive the grace period, not be deleted")
	}
	if after.RuleID == nil || *after.RuleID != *before.RuleID {
		t.Errorf("rule_id = %v, want unchanged %v (grace leaves the row untouched)", after.RuleID, before.RuleID)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("created_at changed from %v to %v: grace should freeze the row, not recreate it",
			before.CreatedAt, after.CreatedAt)
	}
}

// (b) 開始 2 時間前（猶予 1h の外）なら通常どおり削除される。
func TestRunPass_RetractGrace_DoesNotProtectFarFutureUnmatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	const programID = 40002
	insertProgram(t, pool, ctx, programID, "対象番組2", start)

	ruleID := insertRule(t, pool, ctx, "grace-far-future", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組2")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	if _, ok := getReservation(t, pool, ctx, programID); !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}

	retractGraceUnmatch(t, pool, ctx, programID, "非マッチ番組2", start)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after unmatch: %v", err)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Error("reservation starting outside the grace window should have been deleted")
	}
}

// (c) 開始 30 分前でも、ルールを enabled=false にすると猶予の対象外で削除される
// （denpa と同じく「ルールごと削除・停止されたぶんは直前でも引っ込める」。人が
// 押した結果だから）。
func TestRunPass_RetractGrace_DisabledRuleIsNotProtected(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	const programID = 40003
	insertProgram(t, pool, ctx, programID, "対象番組3", start)

	ruleID := insertRule(t, pool, ctx, "grace-disabled-rule", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組3")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	if _, ok := getReservation(t, pool, ctx, programID); !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}

	// タイトルは変えず、ルールそのものを無効化する。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, ruleID); err != nil {
		t.Fatal(err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after disabling rule: %v", err)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Error("reservation backed by a disabled rule must be deleted even within the grace window")
	}
}

// (d) retract_grace: 0（無効）なら、(a) と同じ「開始直前の unmatch」でも通常どおり
// 削除される。
func TestRunPass_RetractGrace_ZeroDisablesProtection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	const programID = 40004
	insertProgram(t, pool, ctx, programID, "対象番組4", start)

	ruleID := insertRule(t, pool, ctx, "grace-zero", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組4")

	// RetractGrace を明示的に指定しない（ゼロ値 = 無効。ruler.Config.RetractGrace の
	// フィールドコメント参照）。ruler.New(sites, pool, nil) と等価だが、意図を明示
	// するため &ruler.Config{} を渡す。
	r := ruler.New([]string{testSite}, pool, &ruler.Config{})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	if _, ok := getReservation(t, pool, ctx, programID); !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}

	retractGraceUnmatch(t, pool, ctx, programID, "非マッチ番組4", start)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after unmatch: %v", err)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Error("with retract_grace disabled (0), an imminent unmatch should be deleted as before")
	}
}

// (e) 猶予で残った行は大量削除サーキットブレーカーの削除カウントに入らない。
// MaxDeletesPerPass を全候補数未満に設定し、猶予が無ければ確実に発動する状況を
// 作ったうえで、猶予によって derivedDeletes がゼロになりブレーカーが発動しないこと
// を確認する。
func TestRunPass_RetractGrace_ProtectedRowsDoNotCountTowardBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(30 * time.Minute).Truncate(time.Second)

	ruleID := insertRule(t, pool, ctx, "grace-breaker", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象")

	const n = 3
	for i := range n {
		insertProgram(t, pool, ctx, int64(40100+i), fmt.Sprintf("対象%d", i), start)
	}

	// 閾値 1: 猶予が無ければ n=3 > 1 で確実に発動する。
	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour, MaxDeletesPerPass: 1})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	for i := range n {
		if _, ok := getReservation(t, pool, ctx, int64(40100+i)); !ok {
			t.Fatalf("program %d should have a reservation before the unmatch", i)
		}
	}

	for i := range n {
		retractGraceUnmatch(t, pool, ctx, int64(40100+i), fmt.Sprintf("非マッチ%d", i), start)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after unmatch: %v", err)
	}

	if _, tripped := getRulerDeletesBreaker(t, pool, ctx); tripped {
		t.Error("circuit breaker should NOT trip: all candidates were protected by the grace period, so derivedDeletes must be 0")
	}
	for i := range n {
		if _, ok := getReservation(t, pool, ctx, int64(40100+i)); !ok {
			t.Errorf("program %d should survive the grace period", i)
		}
	}
}

// (f) 罠の確認: 開始時刻を過ぎた予約は猶予の対象にしない。過ぎた行は reconciler の
// allowlist と GC の領分（ruler.md「開始遅延検出器」の `detectStartDelays` と同じ
// 理由）。放送開始済み（start_at が過去）の番組がルールから外れた場合、猶予が
// 有効でも通常どおり削除されることを確認する。
func TestRunPass_RetractGrace_DoesNotProtectPastStart(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	// 既に放送が始まっている（終了はまだ。testDurationMs = 30 分）。
	start := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	const programID = 40006
	insertProgram(t, pool, ctx, programID, "対象番組6", start)

	ruleID := insertRule(t, pool, ctx, "grace-past-start", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組6")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	if _, ok := getReservation(t, pool, ctx, programID); !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}

	retractGraceUnmatch(t, pool, ctx, programID, "非マッチ番組6", start)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after unmatch: %v", err)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Error("a reservation whose broadcast has already started must not be protected by the grace period")
	}
}

// (g) ユーザーの明示的な skip 意図は、猶予の対象内（放送開始直前・ルール有効）でも
// 必ず解放される。ルールはまだマッチしたままなので rule_id は前パスから NOT NULL
// のままだが、それは「ルールから外れた」のではなく「ユーザーが録るなと押した」の
// で、猶予が守るべき対象ではない（denpa の非対称「ルールごと削除・停止されたぶん
// は直前でも引っ込める」の、予約単位版）。
//
// 罠の確認: 猶予を DeleteReleasedReservationsBySiteAndProgramIDs より前
// （toDelete 全体）に適用すると、rule_id が非 NULL のままのこの行まで猶予が守って
// しまい、このテストは落ちる。
func TestRunPass_RetractGrace_SkipIntentStillReleases(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	const programID = 40007
	insertProgram(t, pool, ctx, programID, "対象番組7", start)

	ruleID := insertRule(t, pool, ctx, "grace-skip-intent", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組7")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	res, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}
	if res.RuleID == nil {
		t.Fatal("reservation should carry a rule_id (otherwise this test asserts nothing)")
	}

	// タイトルは変えない（ルールはまだマッチする）。ユーザーが直前でも明示的に
	// 「これは録らない」と押す。
	q := sqlcgen.New(pool)
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{Site: testSite, ProgramID: programID}); err != nil {
		t.Fatal(err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after skip intent: %v", err)
	}
	if reservationExists(t, pool, ctx, programID) {
		t.Error("an explicit user skip must still release the reservation, even within the retract grace window")
	}
}

// (h) issue #540: 猶予の判定は program_snapshots.start_at ではなく
// epg_programs.start_at（射影の最新値）を直接見る設計になっている。
//
// **この RunPass 経由のテストはもうその設計判断そのものは検証できない**:
// issue #556 で UpsertProgramSnapshotsFromProjection の対象が「射影にまだ居る
// 予約すべて」に広がったため、同じ tx 内で猶予の判定（tq.
// ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs 呼び出し）より前に
// program_snapshots も liveStart に追従済みになる。つまり判定が
// program_snapshots.start_at を見ても epg_programs.start_at を見ても、この
// シナリオでは同じ結果になる（mutation: 判定を program_snapshots 基準に戻し
// てもこのテストは落ちない）。epg_programs を直接見る設計そのものの検証は
// TestListRetractGraceProtectedProgramIDsBySiteAndProgramIDs_UsesLiveEpgStartAt
// （program_snapshots を意図的に stale なまま保つクエリ単体テスト）が担う。
//
// このテストが今も確認するのは (1) 繰り上げ + 改題で unmatch になった予約が
// 猶予で削除されず同じ id のまま残ること、(2) その program_snapshots が
// liveStart に追従すること（issue #556）の 2 点。
func TestRunPass_RetractGrace_UsesLiveStartAtNotFrozenSnapshot(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	staleStart := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	const programID = 40008
	insertProgram(t, pool, ctx, programID, "対象番組8", staleStart)

	ruleID := insertRule(t, pool, ctx, "grace-live-start-forward", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組8")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	before, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}
	if before.RuleID == nil {
		t.Fatal("reservation should carry a rule_id (otherwise this test asserts nothing)")
	}
	if !before.ProgramStartAt.Equal(staleStart) {
		t.Fatalf("program_snapshots.start_at = %v, want %v (fixture setup assertion)", before.ProgramStartAt, staleStart)
	}

	// 同じ EPG 更新で繰り上げ + 改題を同時に行う。unmatch でも番組はまだ射影に
	// 居る（retractGraceUnmatch は epg_programs を更新するだけで削除しない）ので、
	// program_snapshots はこのパスで liveStart に追従する（issue #556。行が
	// まだ existingSet に居る限り desired かどうかを問わず追従させる）。
	liveStart := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	retractGraceUnmatch(t, pool, ctx, programID, "非マッチ番組8", liveStart)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after forward-shift unmatch: %v", err)
	}

	after, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should survive: the live (epg_programs) start time is imminent, even though program_snapshots still showed 5 hours out before this pass")
	}
	// program_snapshots は「射影にまだ居る予約すべて」の対象になるので、猶予で
	// 残った unmatch の行でも追従する（issue #556）。凍結が起きるのは射影から
	// 番組そのものが消えたときだけ（TestRunPass_SnapshotFollowsProjectionThenFreezes
	// が確認する）。
	if !after.ProgramStartAt.Equal(liveStart) {
		t.Errorf("program_snapshots.start_at = %v, want %v (snapshot must follow the live epg_programs value even on the unmatch/grace path)", after.ProgramStartAt, liveStart)
	}
	if after.Title != "非マッチ番組8" {
		t.Errorf("program_snapshots.title = %q, want %q (snapshot must follow the live epg_programs value even on the unmatch/grace path)", after.Title, "非マッチ番組8")
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("created_at changed from %v to %v: grace should freeze the reservation row, not recreate it",
			before.CreatedAt, after.CreatedAt)
	}
}

// issue #540 の設計判断（猶予の判定は epg_programs.start_at を直接見る。
// program_snapshots は見ない）を、RunPass を介さずクエリ単体で検証する。
// program_snapshots.start_at をわざと stale なまま（epg_programs.start_at とは
// 別の値に）保つことで、判定が実際にどちらの列を見ているかを区別できる。
// mutation: SQL の join を program_snapshots に戻すと、stale な値
// （窓の外の +5h）で判定されて結果が空になり、このテストは落ちる。
func TestListRetractGraceProtectedProgramIDsBySiteAndProgramIDs_UsesLiveEpgStartAt(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	const programID = 40011
	liveStart := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	insertProgram(t, pool, ctx, programID, "対象番組11", liveStart)

	ruleID := insertRule(t, pool, ctx, "grace-query-level", 10)

	// RunPass を通さず reservations / program_snapshots を直接組み立てる ---
	// ruler の追従（issue #556）を経由すると program_snapshots も liveStart に
	// 揃ってしまい、判定がどちらの列を見ているか区別できなくなる。
	insertReservationDirect(t, pool, ctx, programID, "対象番組11", liveStart)
	if _, err := pool.Exec(ctx, `UPDATE reservations SET rule_id = $1 WHERE site = $2 AND program_id = $3`,
		ruleID, testSite, programID); err != nil {
		t.Fatalf("setting rule_id fixture: %v", err)
	}
	staleStart := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	if _, err := pool.Exec(ctx, `UPDATE program_snapshots SET start_at = $1 WHERE site = $2 AND program_id = $3`,
		staleStart, testSite, programID); err != nil {
		t.Fatalf("staling program_snapshots fixture: %v", err)
	}

	q := sqlcgen.New(pool)
	now := time.Now()
	got, err := q.ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListRetractGraceProtectedProgramIDsBySiteAndProgramIDsParams{
		Site:       testSite,
		ProgramIds: []int64{programID},
		Now:        now,
		GraceUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs: %v", err)
	}
	if len(got) != 1 || got[0] != programID {
		t.Errorf("got %v, want [%d] — program_snapshots.start_at is stale at +5h (outside the grace window), "+
			"but epg_programs.start_at is live at +20m (inside it); the query must use the live value", got, programID)
	}
}

// (i) 猶予の窓の**上限**を固定する: 番組が開始 5 時間後（窓の外）に後ろ倒しされた
// 予約は削除される。program_snapshots はこのパスで delayedStart に追従するが
// （issue #556）、判定は epg_programs.start_at を直接見るので、それに引きずられて
// 誤って保護されることはない --- 追従の有無に関わらず窓の外なら削除される。
func TestRunPass_RetractGrace_LiveStartAtWinsOverStaleSnapshotWhenDelayed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	imminentStart := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	const programID = 40009
	insertProgram(t, pool, ctx, programID, "対象番組9", imminentStart)

	ruleID := insertRule(t, pool, ctx, "grace-live-start-delay", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "対象番組9")

	r := ruler.New([]string{testSite}, pool, &ruler.Config{RetractGrace: time.Hour})
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("initial RunPass: %v", err)
	}
	before, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation should be created initially (rule matches)")
	}
	if !before.ProgramStartAt.Equal(imminentStart) {
		t.Fatalf("program_snapshots.start_at = %v, want %v (fixture setup assertion)", before.ProgramStartAt, imminentStart)
	}

	// 同じ EPG 更新で後ろ倒し + 改題を同時に行う。番組はまだ射影に居るので
	// program_snapshots も delayedStart に追従する（issue #556）が、このテストが
	// 見たいのは削除される/されないだけで program_snapshots の値は問わない。
	delayedStart := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	retractGraceUnmatch(t, pool, ctx, programID, "非マッチ番組9", delayedStart)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass after delaying unmatch: %v", err)
	}

	if reservationExists(t, pool, ctx, programID) {
		t.Error("reservation should have been deleted: the live (epg_programs) start time is 5 hours out, outside the grace window (program_snapshots follows to the same value, but that must not matter)")
	}
}
