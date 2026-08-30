package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// reservationSourceResp は本ファイルの回帰テストで確認したい source フィールド
// だけを持つ。reservationResp（reservations_overrides_test.go）は source を含めて
// いないため、ここでは別に最小のデコード用型を定義する。
type reservationSourceResp struct {
	Id     int64  `json:"id"`
	Source string `json:"source"`
}

func getReservationJSON(t *testing.T, srv *httptest.Server, programID int64) reservationSourceResp {
	t.Helper()
	resp, err := http.Get(srv.URL + reservationPath(programID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", reservationPath(programID), resp.StatusCode)
	}
	var got reservationSourceResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return got
}

// TestGetReservation_SourceManualDespiteRuleMatch は issue #26 の受け入れ基準 4
// （元のバグの表示側の回帰テスト）: 手動予約（program_intents に action=record の
// 行がある）にルールがマッチして rule_id が埋まっていても、API が返す
// Reservation.source は 'manual' のままでなければならない。
//
// 修正前は reservationFromRow が予約行の source 列（ruler が不可逆に 'rule' へ
// 書き換えていた）をそのまま返していたため、手動予約でも表示が 'rule' に
// 化けていた。修正後は source 列自体が無く、program_intents の有無だけを見る。
func TestGetReservation_SourceManualDespiteRuleMatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115021234
	ruleID := insertRuleFixture(t, pool, ctx)
	// rule_id 付きの予約行 = 「ルールが今マッチしている」状態を模す。
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 11500, 1150)

	// 手動予約であることを表す program_intents{record} を足す
	// （このテストの核心: rule_id があっても intent が「手動」を主張する）。
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentRecord,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}

	got := getReservationJSON(t, srv, programID)
	if got.Source != "manual" {
		t.Errorf("source = %q, want %q "+
			"(手動予約にルールがマッチしていても表示は manual のままのはず。issue #26)", got.Source, "manual")
	}
}

// TestGetReservation_SourceRuleWithoutIntent は「ルール由来の予約（program_intents
// 行なし）」の表示が 'rule' になることを確認する対照テスト。
func TestGetReservation_SourceRuleWithoutIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115031234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 11500, 1150)

	got := getReservationJSON(t, srv, programID)
	if got.Source != "rule" {
		t.Errorf("source = %q, want %q", got.Source, "rule")
	}
}
