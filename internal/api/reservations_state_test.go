package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// reservationStateResp は state フィールドだけを見るデコード用型。
type reservationStateResp struct {
	Id    int64  `json:"id"`
	State string `json:"state"`
}

func getReservationStateJSON(t *testing.T, srv *httptest.Server, id int64) reservationStateResp {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/reservations/" + itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/reservations/%d status = %d, want 200", id, resp.StatusCode)
	}
	var got reservationStateResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return got
}

// TestGetReservation_DetachedViaRuleEditOrRuleDelete は #30 症状 1 の回帰テスト。
//
// state 列は #28/#30 で reservations から撤去され、(rule_id, base, orphaned_at)
// から reservationState（internal/api/handler.go）が読むたびに導出するように
// なった。旧実装（撤去済み）は internal/ruler/sql.go の CASE 式が「前パスの
// rule_id」（r.rule_id）を条件にしていた:
//
//	WHEN d.rule_id IS NOT NULL THEN 'active'
//	WHEN r.rule_id IS NOT NULL THEN 'detached'   -- 前パスの rule_id
//	ELSE COALESCE(r.state, 'active')
//
// ルールを「編集」してマッチしなくなった経路では、直前のパスの r.rule_id が
// まだ非 NULL のまま ruler に読まれるので 'detached' 分岐に入れた。しかし
// ルールを「削除」した経路では、reservations.rule_id の FK が
// ON DELETE SET NULL のため、rule_id は ruler が次にパスを回すより**前**に
// 自動的に NULL へ落ちる。その状態で次のパスが CASE を評価すると d.rule_id と
// r.rule_id のどちらも NULL で、'detached' 分岐に一度も入らず 'active' の
// ままだった（#30 症状 1）。
//
// 新しい導出は (rule_id, base) の**現在値**だけを見るので、rule_id がどちらの
// 経路で NULL になったかを区別しない。このテストは「編集」（ruler が
// rule_id を外す）と「削除」（DELETE /api/rules/{id} が実際に rules 行を
// 消し、FK が rule_id を外す）の両方で detached になることを固定する。
func TestGetReservation_DetachedViaRuleEditOrRuleDelete(t *testing.T) {
	t.Run("ルールを編集してマッチしなくなる経路", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		ctx := context.Background()

		router := api.NewRouter(api.RouterConfig{Pool: pool})
		srv := httptest.NewServer(router)
		defer srv.Close()

		const programID int64 = 1990000199011234
		ruleID := insertRuleFixture(t, pool, ctx)
		resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 19900, 1990)
		// ルールが実際にマッチして base を供給していた状態を模す（computeBase が
		// 書くような非自明な値。空オブジェクトでは「base の実体があるか」の
		// 検証として弱い）。
		if _, err := pool.Exec(ctx, `UPDATE reservations SET base = '{"priority":10}'::jsonb WHERE id = $1`, resID); err != nil {
			t.Fatalf("seeding base: %v", err)
		}

		// 反対方向: rule_id がまだ生きている間は detached ではない（対照）。
		if got := getReservationStateJSON(t, srv, resID); got.State == "detached" {
			t.Fatalf("precondition: state should not be detached while rule_id is still set, got %q", got.State)
		}

		// 「編集してマッチしなくなる」は ruler の resolved CTE が rule_id を NULL に
		// 更新する経路。ruler が実際にそう書くことは internal/ruler の
		// TestRunPass_RuleUnmatch_DeleteVsDetach 等が固定しているので、ここでは
		// その結果（rule_id が NULL、base は直前の値のまま凍結）だけを直接作り、
		// api 層の導出だけを見る。
		if _, err := pool.Exec(ctx, `UPDATE reservations SET rule_id = NULL WHERE id = $1`, resID); err != nil {
			t.Fatalf("simulating rule unmatch: %v", err)
		}

		got := getReservationStateJSON(t, srv, resID)
		if got.State != "detached" {
			t.Errorf("state = %q, want %q (rule edited/unmatched path)", got.State, "detached")
		}
	})

	t.Run("ルールを削除する経路（FK の ON DELETE SET NULL）", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		ctx := context.Background()

		router := api.NewRouter(api.RouterConfig{Pool: pool})
		srv := httptest.NewServer(router)
		defer srv.Close()

		const programID int64 = 1990000199021234
		ruleID := insertRuleFixture(t, pool, ctx)
		resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 19900, 1990)
		if _, err := pool.Exec(ctx, `UPDATE reservations SET base = '{"priority":10}'::jsonb WHERE id = $1`, resID); err != nil {
			t.Fatalf("seeding base: %v", err)
		}
		// DeleteRule は投資（program_intents / program_overrides のどちらか）が
		// ない導出予約を物理削除する（internal/api/rules.go の
		// DeleteReservationsByRuleWithoutIntent）。生き残らせて detached にするため、
		// PATCH で program_overrides の投資を作っておく
		// （TestDeleteRule_ReservationWithOnlyOverridesSurvivesDetached と同じ前提）。
		patchResp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"priority":9}`)
		if patchResp.StatusCode != http.StatusOK {
			t.Fatalf("seeding overrides patch status = %d, want 200", patchResp.StatusCode)
		}
		_ = decodeReservation(t, patchResp)

		// 反対方向: ルール削除前は detached ではない（対照）。
		if got := getReservationStateJSON(t, srv, resID); got.State == "detached" {
			t.Fatalf("precondition: state should not be detached before the rule is deleted, got %q", got.State)
		}

		// 核心: 実際に DELETE /api/rules/{id} を叩く。ruler を一切経由せず、
		// reservations.rule_id の FK（ON DELETE SET NULL）だけで rule_id が
		// 外れる。旧実装はここで detached にならなかった（#30 症状 1 本体）。
		delResp := doDelete(t, srv, "/api/rules/"+itoa(ruleID))
		if delResp.StatusCode != http.StatusOK {
			t.Fatalf("delete rule status = %d, want 200", delResp.StatusCode)
		}
		_ = delResp.Body.Close()

		got := getReservationStateJSON(t, srv, resID)
		if got.State != "detached" {
			t.Errorf("state = %q, want %q "+
				"(rule DELETE must also produce detached — this was the #30 symptom-1 bug: "+
				"the old CASE only looked at the previous pass's rule_id, which the FK's "+
				"ON DELETE SET NULL had already cleared before any ruler pass could react to it)",
				got.State, "detached")
		}
	})
}
