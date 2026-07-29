package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// overlapsURL は GET /api/programs/{programId}/overlaps の URL を組む。
func overlapsURL(base string, programID int64) string {
	return fmt.Sprintf("%s/api/programs/%d/overlaps", base, programID)
}

// reserveViaAPI は POST /api/reservations で手動予約を作成し、デコードした
// Reservation を返す（201 でなければ即 Fatal）。
func reserveViaAPI(t *testing.T, srv string, programID int64, title string, start time.Time, durationMs int64) Reservation {
	t.Helper()
	body := fmt.Sprintf(`{"programId":%d,"title":%q,"startAt":%q,"durationMs":%d}`,
		programID, title, start.Format(time.RFC3339), durationMs)
	resp, err := http.Post(srv+"/api/reservations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/reservations: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create reservation for program %d: status = %d, want 201", programID, resp.StatusCode)
	}
	var out Reservation
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding created reservation: %v", err)
	}
	return out
}

// 重なる予約（同時間帯に別の予約がある）は件数に数え、重ならない予約
// （時間がずれている）は数えない。
func TestGetProgramOverlaps_CountsOverlapping(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 200, 32678, 5168, 1, "対象番組", base, false)
	seedEpgProgram(t, pool, 201, 32678, 5168, 2, "重なる番組", base.Add(30*time.Minute), false)
	seedEpgProgram(t, pool, 202, 32678, 5168, 3, "重ならない番組", base.Add(-2*time.Hour), false)

	// 対象番組 [00:00,01:00) と重なる予約 [00:30,01:30)、重ならない予約 [-2:00,-1:00) を用意
	reserveViaAPI(t, srv.URL, 201, "重なる番組", base.Add(30*time.Minute), testProgramDuration.Milliseconds())
	reserveViaAPI(t, srv.URL, 202, "重ならない番組", base.Add(-2*time.Hour), testProgramDuration.Milliseconds())

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
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 210, 32678, 5168, 1, "対象番組", base, false) // [base, base+1h)
	// 前番組: 終了がちょうど対象番組の開始と同時刻
	seedEpgProgram(t, pool, 211, 32678, 5168, 2, "前番組", base.Add(-testProgramDuration), false)
	// 後番組: 開始がちょうど対象番組の終了と同時刻
	seedEpgProgram(t, pool, 212, 32678, 5168, 3, "後番組", base.Add(testProgramDuration), false)

	reserveViaAPI(t, srv.URL, 211, "前番組", base.Add(-testProgramDuration), testProgramDuration.Milliseconds())
	reserveViaAPI(t, srv.URL, 212, "後番組", base.Add(testProgramDuration), testProgramDuration.Milliseconds())

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
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 220, 32678, 5168, 1, "対象番組", base, false)

	reserveViaAPI(t, srv.URL, 220, "対象番組", base, testProgramDuration.Milliseconds())

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 220), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 (自分自身は数えない): %+v", got.Count, got.Reservations)
	}
}

// state = 'orphaned' の予約（番組が終了して録れなかったもの）は重なりの
// 相手にならない。
func TestGetProgramOverlaps_ExcludesOrphaned(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 230, 32678, 5168, 1, "対象番組", base, false)
	seedEpgProgram(t, pool, 231, 32678, 5168, 2, "orphan になる番組", base.Add(30*time.Minute), false)

	orphaned := reserveViaAPI(t, srv.URL, 231, "orphan になる番組", base.Add(30*time.Minute), testProgramDuration.Milliseconds())

	// MarkReservationOrphaned は :execrows になった（internal/reconciler の
	// markOrphaned が実際に更新できた行数をログ出力の可否に使うため）。
	// この呼び出しは常に対象行があるので rows は捨てて良い。
	if _, err := sqlcgen.New(pool).MarkReservationOrphaned(ctx, orphaned.Id); err != nil {
		t.Fatalf("marking reservation orphaned: %v", err)
	}

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 230), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 (orphaned な予約は数えない): %+v", got.Count, got.Reservations)
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
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: defaultSite, ProgramID: skippedProgramID, Action: "skip",
		ProgramStartAt: skippedStart, ProgramDurationMs: testProgramDuration.Milliseconds(),
	}); err != nil {
		t.Fatalf("upserting skip intent: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: defaultSite, ProgramID: skippedProgramID, Overrides: json.RawMessage(`{"priority":9}`),
		ProgramStartAt: skippedStart, ProgramDurationMs: testProgramDuration.Milliseconds(),
	}); err != nil {
		t.Fatalf("upserting program overrides: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (
  site, program_id, state, title, program_start_at, program_duration_ms,
  network_id, service_id, channel_type, channel
) VALUES ('default', $1, 'active', 'skip された番組', $2, $3, 32678, 5168, 'GR', '27')`,
		skippedProgramID, skippedStart, testProgramDuration.Milliseconds()); err != nil {
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
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	seedEpgService(t, pool, 32678, 5168, 8, "テスト局", "27")
	seedEpgProgram(t, pool, 250, 32678, 5168, 1, "対象番組", base, false)
	seedEpgProgram(t, pool, 251, 32678, 5168, 2, "重なる番組", base.Add(30*time.Minute), false)

	overlapping := reserveViaAPI(t, srv.URL, 251, "重なる番組", base.Add(30*time.Minute), testProgramDuration.Milliseconds())

	var got ProgramOverlaps
	resp := getJSON(t, overlapsURL(srv.URL, 250), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(got.Reservations) != 1 {
		t.Fatalf("reservations = %+v, want 1 entry", got.Reservations)
	}
	entry := got.Reservations[0]
	if entry.Id != overlapping.Id {
		t.Errorf("id = %d, want %d", entry.Id, overlapping.Id)
	}
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
