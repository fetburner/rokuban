package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// overlapsURL は GET /api/sites/{site}/programs/{programId}/overlaps の URL を組む。
func overlapsURL(base string, programID int64) string {
	return fmt.Sprintf("%s/api/sites/default/programs/%d/overlaps", base, programID)
}

// reserveViaAPI は PUT .../intent {action:"record"} で意図を立ててから、ruler が
// 本来行う reservations 行の実体化を模して直接 1 行 INSERT する
// （テストでは ruler パスを回さないため）。番組の事実（title / 開始時刻 / 尺）は
// intent の書き込みが EPG プロジェクションから program_snapshots に写すので、
// 呼び出し側は事前に seedEpgProgram で対象番組を登録しておくこと。
func reserveViaAPI(t *testing.T, srv string, pool *pgxpool.Pool, ctx context.Context, programID int64) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/sites/default/programs/%d/intent", srv, programID),
		strings.NewReader(`{"action":"record"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT intent for program %d: %v", programID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent for program %d: status = %d, want 204", programID, resp.StatusCode)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id) VALUES ('default', $1)`,
		programID); err != nil {
		t.Fatalf("inserting reservation fixture for program %d: %v", programID, err)
	}
}

// seedNeverScheduledEvent は reconciler.recordNeverScheduled が実際に書く
// never_scheduled_events 行を模す。GetProgramOverlaps 等、この行の存在に依存
// する API 層の挙動を確認するための直接 INSERT（reconciler パスは回さない）。
func seedNeverScheduledEvent(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	site string, networkID, serviceID, eventID int32,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO never_scheduled_events (site, network_id, service_id, event_id)
VALUES ($1, $2, $3, $4)`, site, networkID, serviceID, eventID); err != nil {
		t.Fatalf("seeding never-scheduled event: %v", err)
	}
}

// 重なる予約（同時間帯に別の予約がある）は件数に数え、重ならない予約
// （時間がずれている）は数えない。
func TestGetProgramOverlaps_CountsOverlapping(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 200, 32678, 5168, 1, "対象番組", base, false)
	seedEpgProgram(t, pool, 201, 32678, 5168, 2, "重なる番組", base.Add(30*time.Minute), false)
	seedEpgProgram(t, pool, 202, 32678, 5168, 3, "重ならない番組", base.Add(-2*time.Hour), false)

	// 対象番組 [00:00,01:00) と重なる予約 [00:30,01:30)、重ならない予約 [-2:00,-1:00) を用意
	reserveViaAPI(t, srv.URL, pool, ctx, 201)
	reserveViaAPI(t, srv.URL, pool, ctx, 202)

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 200), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 1 {
		t.Fatalf("count = %d, want 1 (重なる番組の予約 1 件だけ)", got.Count)
	}
	if len(got.Reservations) != 1 || got.Reservations[0].Title != "重なる番組" {
		t.Errorf("reservations = %+v, want [重なる番組]", got.Reservations)
	}
}

// 接している番組（前番組の終了 = 次番組の開始）は重なりと数えない。番組表は
// 連続しているため、これが漏れると隣接番組がすべて重なりになってしまう
// （閉区間ではなく半開区間で判定する必要がある核心のテスト）。
func TestGetProgramOverlaps_AdjacentProgramNotCounted(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 210, 32678, 5168, 1, "対象番組", base, false) // [base, base+1h)
	// 前番組: 終了がちょうど対象番組の開始と同時刻
	seedEpgProgram(t, pool, 211, 32678, 5168, 2, "前番組", base.Add(-testProgramDuration), false)
	// 後番組: 開始がちょうど対象番組の終了と同時刻
	seedEpgProgram(t, pool, 212, 32678, 5168, 3, "後番組", base.Add(testProgramDuration), false)

	reserveViaAPI(t, srv.URL, pool, ctx, 211)
	reserveViaAPI(t, srv.URL, pool, ctx, 212)

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 210), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 (前後番組は接しているだけで重ならない): %+v", got.Count, got.Reservations)
	}
}

// 自分自身（同じ programId の既存予約）は重なりに数えない。既にその番組を
// 予約済みの場合に「自分と重なっている」と出るのは無意味なので除外する。
func TestGetProgramOverlaps_ExcludesSelf(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 220, 32678, 5168, 1, "対象番組", base, false)

	reserveViaAPI(t, srv.URL, pool, ctx, 220)

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 220), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 (自分自身は数えない): %+v", got.Count, got.Reservations)
	}
}

// 予約に紐づく欠測行（never_scheduled_events。「番組終了時点で schedule が
// 一度も観測されなかった」という reconciler の観測）がある予約は重なりの相手に
// ならない。reconciler.recordNeverScheduled が実際に書く欠測行を模す。
func TestGetProgramOverlaps_ExcludesNeverScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 230, 32678, 5168, 1, "対象番組", base, false)
	seedEpgProgram(t, pool, 231, 32678, 5168, 2, "never-scheduled になる番組", base.Add(30*time.Minute), false)

	reserveViaAPI(t, srv.URL, pool, ctx, 231)
	seedNeverScheduledEvent(t, pool, ctx, "default", 32678, 5168, 2)

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 230), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 (never-scheduled な予約は数えない): %+v", got.Count, got.Reservations)
	}
}

// 放送中の mirakc 由来の失敗は欠測表に入らず同期除外の対象にならないので、
// 重なり判定でも数える。TestGetProgramOverlaps_ExcludesNeverScheduled と対になる
// 反転テスト。
func TestGetProgramOverlaps_MidRecordingFailureNotExcluded(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32679, 5169, 9, "テスト局", "27")
	seedEpgProgram(t, pool, 240, 32679, 5169, 3, "対象番組2", base, false)
	seedEpgProgram(t, pool, 241, 32679, 5169, 4, "放送中に失敗した番組", base.Add(30*time.Minute), false)

	reserveViaAPI(t, srv.URL, pool, ctx, 241)
	// handleRecordingFailed が作る形の failed 行（never-scheduled マーカーは無い）。
	if _, err := pool.Exec(ctx, `
INSERT INTO recordings (
    source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, program_start_at, program_duration_ms, status, quality_events
) VALUES ('manual', 'default', 32679, 5169, 4, 'テスト局', 'GR', '27', '放送中に失敗した番組', $1, $2, 'failed',
    '[{"event":"recording.failed","reason":"need-rescheduling"}]'::jsonb)`,
		base.Add(30*time.Minute), time.Hour.Milliseconds()); err != nil {
		t.Fatalf("seeding mid-recording failure: %v", err)
	}

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 240), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 1 {
		t.Errorf("count = %d, want 1 (never-scheduled マーカー無しの failed 行は除外してはいけない): %+v", got.Count, got.Reservations)
	}
}

// effective.skip が true の予約は重なりに数えない。
//
// 到達可能性の確認: DeleteReservation は program_intents.action='skip' を書いて
// reservations 行を削除する一方、program_overrides の行には一切触れない
// （TestDeleteReservation_KeepsSkipIntent 参照）。internal/ruler/ruler.go の
// 全量パスは「desired = (ルールにマッチ ∪ intent.record) − intent.skip」を
// 計算したあと、**program_overrides に行がある番組を無条件に desired へ戻す**
// （同ファイルの `overrideProgramIDs` を desired に足す行、コメント
// 「overrides の存在は skip 意図があっても desired に残す」）。したがって
// 「override 付きの手動予約を作って取消す」→ 次のルールパスで
// reservations 行が復活する、という経路で「reservations 行が存在するのに
// effective.skip=true」という状態が実際に起こりうる。
//
// ここでは River ジョブ経由で実際に ruler パスを回す代わりに、上記の経路が
// 最終的に作る DB 状態（reservations 行 + program_intents.action='skip' +
// program_overrides 行）を直接組み立てて、api 層の判定だけを確かめる。
func TestGetProgramOverlaps_ExcludesSkippedIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)
	q := sqlcgen.New(pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 240, 32678, 5168, 1, "対象番組", base, false)
	const skippedProgramID int64 = 241
	seedEpgProgram(t, pool, skippedProgramID, 32678, 5168, 2, "skip された番組", base.Add(30*time.Minute), false)

	skippedStart := base.Add(30 * time.Minute)
	networkID, serviceID := int32(32678), int32(5168)
	channelType, channel := "GR", "27"
	// program_intents / program_overrides / reservations はいずれも
	// program_snapshots への FK（#27）を持つので、先に upsert しておく。
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site: db.DefaultSite, ProgramID: skippedProgramID, Title: "skip された番組",
		StartAt: skippedStart, DurationMs: testProgramDuration.Milliseconds(),
		NetworkID: networkID, ServiceID: serviceID, ChannelType: channelType, Channel: channel,
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: db.DefaultSite, ProgramID: skippedProgramID, Action: "skip",
	}); err != nil {
		t.Fatalf("upserting skip intent: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: db.DefaultSite, ProgramID: skippedProgramID, Overrides: json.RawMessage(`{"priority":9}`),
	}); err != nil {
		t.Fatalf("upserting program overrides: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id) VALUES ('default', $1)`,
		skippedProgramID); err != nil {
		t.Fatalf("inserting reservation row for skipped program: %v", err)
	}

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 240), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 (effective.skip=true の予約は数えない): %+v", got.Count, got.Reservations)
	}
}

// 内訳（title と時刻）が返ること。件数だけでは「何と重なっているか」が
// 分からずユーザーが判断できない。
func TestGetProgramOverlaps_ReturnsBreakdown(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 250, 32678, 5168, 1, "対象番組", base, false)
	seedEpgProgram(t, pool, 251, 32678, 5168, 2, "重なる番組", base.Add(30*time.Minute), false)

	reserveViaAPI(t, srv.URL, pool, ctx, 251)

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 250), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(got.Reservations) != 1 {
		t.Fatalf("reservations = %+v, want 1 entry", got.Reservations)
	}
	entry := got.Reservations[0]
	if entry.ProgramId != 251 {
		t.Errorf("programId = %d, want 251", entry.ProgramId)
	}
	if entry.Title != "重なる番組" {
		t.Errorf("title = %q, want 重なる番組", entry.Title)
	}
	if !entry.StartAt.Equal(base.Add(30 * time.Minute)) {
		t.Errorf("startAt = %v, want %v", entry.StartAt, base.Add(30*time.Minute))
	}
	if entry.DurationMs != testProgramDuration.Milliseconds() {
		t.Errorf("durationMs = %d, want %d", entry.DurationMs, testProgramDuration.Milliseconds())
	}
}

// 番組が EPG プロジェクションに無ければ 404。
func TestGetProgramOverlaps_ProgramNotInProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	resp := getJSON(t, overlapsURL(srv.URL, 999999), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
