package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// insertProgramSnapshotFixture は program_snapshots にだけ行を作る（reservations
// には触れない）。ruler の導出削除・再実体化（DELETE→CreateManualReservation）を
// 模すには、program_snapshots は 1 回だけ作って reservations 側だけを作り直す
// 必要があるため、insertReservationDirect（program_snapshots + reservations を
// まとめて作る）とは別に切り出す。
func insertProgramSnapshotFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, networkID, serviceID int32) {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	// 識別 6 列はすべて NOT NULL（issue #101）。event_id / service_name を
	// 落とすと 23502 で落ちる —— このフィクスチャは #99（PR #147）で追加され、
	// #101（PR #150）が NOT NULL 化を入れた。互いのブランチでは緑で、マージ後に
	// 初めて落ちた（CLAUDE.md §並行作業「git が競合と見なさない意味的な競合」の
	// テストフィクスチャの生 SQL の実例）。
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
    site, program_id, title, start_at, duration_ms,
    network_id, service_id, channel_type, channel, event_id, service_name
) VALUES ('default', $1, 'テスト番組', $2, 1800000, $3, $4, 'GR', '27', $5, 'テスト局')`,
		programID, start, networkID, serviceID, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
}

func programReservationPath(programID int64) string {
	return "/api/sites/default/programs/" + itoa(programID) + "/reservation"
}

// TestGetProgramReservation_SurvivesRematerialization はこの issue (#99) の本体:
// 予約行が ruler の導出削除・再実体化で id を変えても、(site, programId) を
// 宛先にした同じ URL で引けること。
//
// #98 で never-scheduled の除外条件が同じ形のバグ
// （TestReconciler_NeverScheduledExclusionSurvivesRematerialization、
// internal/reconciler/never_scheduled_identity_test.go）を踏んでいる。ここでは
// 読み取り API がその identity 問題を持ち込んでいないことを確認する。
func TestGetProgramReservation_SurvivesRematerialization(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 900000090011234
	insertProgramSnapshotFixture(t, pool, ctx, programID, 90000, 9001)

	res1, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("creating initial reservation: %v", err)
	}

	path := srv.URL + programReservationPath(programID)

	resp1, err := http.Get(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp1.StatusCode)
	}
	var got1 api.Reservation
	if err := json.NewDecoder(resp1.Body).Decode(&got1); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got1.ProgramId != programID {
		t.Fatalf("programId = %d, want %d (initial reservation)", got1.ProgramId, programID)
	}

	// ruler の導出削除 → 再実体化を模す（同じ番組・同じ複合キー）。program_snapshots
	// は放送が続く限り生きているので作り直さない（#27 の分離の帰結）。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, res1.Site, res1.ProgramID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	_, err = q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("re-materializing reservation: %v", err)
	}

	// 核心: 同じ URL（(site, programId) 宛先）でもう一度引くと、再実体化された
	// 予約が 200 で返る。複合キー以外を宛先にしていれば、ここは
	// 404（旧 id はもう存在しない）になっていたはずの経路。
	resp2, err := http.Get(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("再実体化後の status = %d, want 200（(site, programId) は変わらないはず）", resp2.StatusCode)
	}
	var got2 api.Reservation
	if err := json.NewDecoder(resp2.Body).Decode(&got2); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got2.ProgramId != programID {
		t.Errorf("programId = %d, want %d", got2.ProgramId, programID)
	}
}

// 指定した (site, programId) に予約が無ければ 404。
func TestGetProgramReservation_NoReservation_Returns404(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 900000090021234
	insertProgramSnapshotFixture(t, pool, ctx, programID, 90000, 9002)

	resp, err := http.Get(srv.URL + programReservationPath(programID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no reservation for this program)", resp.StatusCode)
	}
}

// site がプロセスの設定と一致しなければ 404（他の GET 系エンドポイントと同じ
// 規約。GetProgram / GetProgramOverlaps 等）。
func TestGetProgramReservation_UnknownSite_Returns404(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 900000090031234
	insertProgramSnapshotFixture(t, pool, ctx, programID, 90000, 9003)
	if _, err := sqlcgen.New(pool).CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: "default", ProgramID: programID,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/sites/other-site/programs/" + itoa(programID) + "/reservation")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (unknown site)", resp.StatusCode)
	}
}
