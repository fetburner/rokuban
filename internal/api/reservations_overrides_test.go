package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// reservationResp は overrides API のレスポンスから確認に要る部分だけを持つ。
type reservationResp struct {
	Id        int64                  `json:"id"`
	RuleId    *int64                 `json:"ruleId"`
	Overrides map[string]interface{} `json:"overrides"`
}

// insertRuleFixture は最小構成のルールを 1 件作る。
func insertRuleFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO rules (name) VALUES ('テストルール') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	return id
}

// insertReservationDirect は reservations 行を直接作る（ruler を経由しない）。
// ruleID の有無で「ルール由来」「手動」を模した行を作るが、program_intents には
// 触れない（reservations.source 列は issue #26 で削除済み。この直生成では
// intent が無いため、reservationFromRow 経由の API 表示は常に rule になる）。
// overrides API は program_overrides だけを書くので、reservations 側の
// セットアップはこの生 SQL で足りる。
func insertReservationDirect(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, ruleID *int64, networkID, serviceID int32) int64 {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	var id int64
	err := pool.QueryRow(ctx, `
INSERT INTO reservations (
  site, program_id, rule_id, state, base, title,
  program_start_at, program_duration_ms,
  network_id, service_id, channel_type, channel
) VALUES (
  'default', $1, $2, 'active', '{}'::jsonb, 'テスト番組',
  $3, 1800000, $4, $5, 'GR', '27'
) RETURNING id`, programID, ruleID, start, networkID, serviceID).Scan(&id)
	if err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
	return id
}

func doPatch(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func doDelete(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeReservation(t *testing.T, resp *http.Response) reservationResp {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out reservationResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return out
}

// 1. ルール由来の予約に PATCH すると program_overrides に行が新規作成される。
// **program_intents には行ができない**（これが分離の核心。1 表だった頃は
// action='record' が立っていた。docs/recording.md §4.2）。
func TestUpdateReservationOverrides_CreatesOverrideForRuleReservation_NotIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1100000110011234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 11000, 1100)

	// 事前に意図も上書きも存在しない。
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Fatalf("intent should not exist before patch, got err=%v", err)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Fatalf("overrides should not exist before patch, got err=%v", err)
	}

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"priority":7}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	got := decodeReservation(t, resp)
	if got.Overrides == nil || intJSONNumber(t, got.Overrides["priority"]) != 7 {
		t.Errorf("response overrides = %+v, want priority=7", got.Overrides)
	}

	overrides, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID})
	if err != nil {
		t.Fatalf("overrides after patch: %v", err)
	}
	if got := overridesPriority(t, overrides.Overrides); got != 7 {
		t.Errorf("overrides priority = %d, want 7 (%s)", got, overrides.Overrides)
	}

	// 核心: program_intents には一切行ができない。
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("program_intents should still not exist after patching a rule-derived reservation's overrides, err=%v", err)
	}
}

// 2. 分離前のバグの回帰テスト: 手動予約に後からルールがマッチした状態で
// 「ルールに戻す」をしても program_intents の行が残り、予約も残る。
//
// 1 表だった頃は、上書きが空になると「rule_id が付いているか」で意図の行を
// 掃除していた。手動予約（intent{record}）にルールがマッチして rule_id が
// 埋まった状態で「ルールに戻す」を押すと、掃除規則が発火して意図の行が
// 消え、その後ルールが外れると手動予約が消えてしまうバグがあった
// （docs/recording.md §4.2「overrides は program_intents とは別の表に置く」）。
// 分離後は DELETE /overrides が program_intents に一切触れないので、
// この経路は構造的に存在しない。
func TestResetReservationOverrides_DoesNotTouchProgramIntents_ManualReservationSurvives(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115011234
	// 手動予約に後からルールがマッチした状態を直接作る: rule_id が付いた
	// reservation 行 + intent{record}。
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 11500, 1150)

	var startAt time.Time
	var durationMs int64
	if err := pool.QueryRow(ctx,
		`SELECT program_start_at, program_duration_ms FROM reservations WHERE id = $1`, resID,
	).Scan(&startAt, &durationMs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentRecord,
		ProgramStartAt: startAt, ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID, Overrides: []byte(`{"priority":5}`),
		ProgramStartAt: startAt, ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doDelete(t, srv, "/api/reservations/"+itoa(resID)+"/overrides")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete overrides status = %d, want 200", resp.StatusCode)
	}
	got := decodeReservation(t, resp)
	if len(got.Overrides) != 0 {
		t.Errorf("overrides should be empty, got %+v", got.Overrides)
	}

	// program_overrides の行は消える。
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("overrides row should be deleted, err=%v", err)
	}

	// 核心: program_intents の行は残る（DELETE /overrides は触らない）。
	intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: "default", ProgramID: programID})
	if err != nil {
		t.Fatalf("program_intents row must survive resetting overrides "+
			"(the old single-table cleanup rule would have deleted it here): %v", err)
	}
	if intent.Action != db.IntentRecord {
		t.Errorf("action = %q, want record", intent.Action)
	}

	// 核心: 予約行も残る（意図が生きている限り、次の ruler パスでも消えない）。
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE id = $1`, resID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reservation row count = %d, want 1 (manual reservation must survive)", n)
	}
}

// 3. 何も指定しない PATCH {} が program_intents を変更しない
// （分離前は掃除規則が発火して意図を消しえた）。
func TestUpdateReservationOverrides_EmptyPatch_DoesNotTouchProgramIntents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 手動予約 + ルールが後からマッチした状況（rule_id 付き）を再現する。
	const programID int64 = 1160000116011234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 11600, 1160)

	var startAt time.Time
	var durationMs int64
	if err := pool.QueryRow(ctx,
		`SELECT program_start_at, program_duration_ms FROM reservations WHERE id = $1`, resID,
	).Scan(&startAt, &durationMs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentRecord,
		ProgramStartAt: startAt, ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}

	for _, body := range []string{`{}`, `{"reset":[]}`} {
		t.Run(body, func(t *testing.T) {
			resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), body)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("patch %s status = %d, want 200 (empty patch is a harmless no-op)", body, resp.StatusCode)
			}
			_ = decodeReservation(t, resp)

			intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{
				Site: "default", ProgramID: programID,
			})
			if err != nil {
				t.Fatalf("no-op PATCH must not delete the intent row: %v", err)
			}
			if intent.Action != db.IntentRecord {
				t.Errorf("action = %q, want record", intent.Action)
			}
		})
	}
}

// 4. ruler が program_overrides を書かないことは internal/ruler のテストで
// 固定してある（TestRunPass_DoesNotWriteProgramOverrides）。ここでは api 側の
// 経路として、PATCH が意図した通り program_overrides の行だけを作ることを
// 確認する（同じ確認は 1 番のテストにも含まれる）。

// 5. program_overrides に行があるだけで ruler の desired に入ることは
// internal/ruler のテストで固定してある
// （TestRunPass_OverridesAloneKeepReservationDetached）。

// 6. GC で番組終了後の program_overrides 行が消えることは internal/ruler の
// テストで固定してある（TestRunPass_GC_DeletesEndedPastGrace）。

// 7. 部分更新（指定していないフィールドの override が保たれる）。
func TestUpdateReservationOverrides_PartialUpdatePreservesOtherFields(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1200000120021234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 12000, 1200)

	var startAt time.Time
	var durationMs int64
	if err := pool.QueryRow(ctx,
		`SELECT program_start_at, program_duration_ms FROM reservations WHERE id = $1`, resID,
	).Scan(&startAt, &durationMs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides:         []byte(`{"priority":5,"keepOriginal":"always"}`),
		ProgramStartAt:    startAt,
		ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"encodeProfiles":["h264"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	got := decodeReservation(t, resp)
	if intJSONNumber(t, got.Overrides["priority"]) != 5 {
		t.Errorf("priority should be preserved, got %+v", got.Overrides)
	}
	if got.Overrides["keepOriginal"] != "always" {
		t.Errorf("keepOriginal should be preserved, got %+v", got.Overrides)
	}
	profiles, ok := got.Overrides["encodeProfiles"].([]interface{})
	if !ok || len(profiles) != 1 || profiles[0] != "h264" {
		t.Errorf("encodeProfiles should be set, got %+v", got.Overrides)
	}
}

// 8. reset でフィールド単位に override が消え、他は残る。
func TestUpdateReservationOverrides_ResetRemovesOnlyThatField(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1300000130031234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 13000, 1300)

	var startAt time.Time
	var durationMs int64
	if err := pool.QueryRow(ctx,
		`SELECT program_start_at, program_duration_ms FROM reservations WHERE id = $1`, resID,
	).Scan(&startAt, &durationMs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides:         []byte(`{"priority":5,"keepOriginal":"always"}`),
		ProgramStartAt:    startAt,
		ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"reset":["priority"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	got := decodeReservation(t, resp)
	if _, ok := got.Overrides["priority"]; ok {
		t.Errorf("priority should be reset away, got %+v", got.Overrides)
	}
	if got.Overrides["keepOriginal"] != "always" {
		t.Errorf("keepOriginal should be preserved, got %+v", got.Overrides)
	}

	overrides, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID})
	if err != nil {
		t.Fatalf("overrides row should still exist (not empty): %v", err)
	}
	if _, ok := jsonMap(t, overrides.Overrides)["priority"]; ok {
		t.Errorf("stored overrides still has priority: %s", overrides.Overrides)
	}
}

// 9. reset で最後の override が消えたら program_overrides の行自体が消える。
func TestUpdateReservationOverrides_ResetLastField_DeletesOverridesRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1400000140041234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 14000, 1400)

	var startAt time.Time
	var durationMs int64
	if err := pool.QueryRow(ctx,
		`SELECT program_start_at, program_duration_ms FROM reservations WHERE id = $1`, resID,
	).Scan(&startAt, &durationMs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides:         []byte(`{"priority":5}`),
		ProgramStartAt:    startAt,
		ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"reset":["priority"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	got := decodeReservation(t, resp)
	if len(got.Overrides) != 0 {
		t.Errorf("overrides should be empty, got %+v", got.Overrides)
	}

	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("program_overrides row should be deleted when overrides become empty, err=%v", err)
	}

	// 予約行自体はこの操作では触らない（削除は次の ruler パスの仕事）。
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE id = $1`, resID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reservation row count = %d, want 1 (PATCH must not touch reservations)", n)
	}
}

// 10. DELETE /overrides が全部消す。
func TestResetReservationOverrides_ClearsAll(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1600000160061234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 16000, 1600)

	var startAt time.Time
	var durationMs int64
	if err := pool.QueryRow(ctx,
		`SELECT program_start_at, program_duration_ms FROM reservations WHERE id = $1`, resID,
	).Scan(&startAt, &durationMs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides:         []byte(`{"priority":5,"keepOriginal":"always"}`),
		ProgramStartAt:    startAt,
		ProgramDurationMs: durationMs,
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doDelete(t, srv, "/api/reservations/"+itoa(resID)+"/overrides")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete overrides status = %d, want 200", resp.StatusCode)
	}
	got := decodeReservation(t, resp)
	if len(got.Overrides) != 0 {
		t.Errorf("overrides should be empty, got %+v", got.Overrides)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("overrides row should be deleted, err=%v", err)
	}
}

// 11. 400 群: 値と reset の衝突 / 未知の reset フィールド名 / 不正な keepOriginal /
// 不正な filenameTemplate / 負の priority。
func TestUpdateReservationOverrides_ValueAndResetConflict_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1700000170071234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 17000, 1700)

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"priority":5,"reset":["priority"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("no overrides should be created on a rejected request, err=%v", err)
	}
}

func TestUpdateReservationOverrides_UnknownResetField_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1800000180081234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 18000, 1800)

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"reset":["skip"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (skip is not an overrides key)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp2 := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"reset":["totallyUnknownField"]}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (typo'd field name)", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}

func TestUpdateReservationOverrides_InvalidFields_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1900000190091234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 19000, 1900)

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"keepOriginal":"sometimes"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid keepOriginal: status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp2 := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"filenameTemplate":"{{.NoSuchField}}"}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid filenameTemplate: status = %d, want 400", resp2.StatusCode)
	}
	_ = resp2.Body.Close()

	resp3 := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"priority":-1}`)
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative priority: status = %d, want 400", resp3.StatusCode)
	}
	_ = resp3.Body.Close()
}

// 12. 存在しない予約への PATCH / DELETE overrides が 404。
func TestUpdateAndResetReservationOverrides_NotFound(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const missingID = 999999

	resp := doPatch(t, srv, "/api/reservations/"+itoa(missingID), `{"priority":5}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("patch missing reservation: status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp2 := doDelete(t, srv, "/api/reservations/"+itoa(missingID)+"/overrides")
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("delete overrides on missing reservation: status = %d, want 404", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}

// 13. PATCH の結果が db.EffectiveOptions を通して期待どおりの effective になる。
func TestUpdateReservationOverrides_EffectiveOptionsRoundTrip(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 2100000210111234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 21000, 2100)

	base := []byte(`{"priority":3,"encodeProfiles":["h264"],"keepOriginal":"always"}`)
	if _, err := pool.Exec(ctx, `UPDATE reservations SET base = $1 WHERE id = $2`, base, resID); err != nil {
		t.Fatalf("seeding base: %v", err)
	}

	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"priority":7}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	_ = decodeReservation(t, resp)

	row, err := q.GetReservationFull(ctx, resID)
	if err != nil {
		t.Fatalf("reloading reservation: %v", err)
	}
	eff, err := db.EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
	if err != nil {
		t.Fatalf("computing effective options: %v", err)
	}
	if eff.Priority == nil || *eff.Priority != 7 {
		t.Errorf("effective priority = %v, want 7 (override should win)", eff.Priority)
	}
	if eff.EncodeProfiles == nil || len(*eff.EncodeProfiles) != 1 || (*eff.EncodeProfiles)[0] != "h264" {
		t.Errorf("effective encodeProfiles = %v, want [h264] (base should carry through untouched)", eff.EncodeProfiles)
	}
	if eff.KeepOriginal == nil || *eff.KeepOriginal != "always" {
		t.Errorf("effective keepOriginal = %v, want always (base should carry through untouched)", eff.KeepOriginal)
	}
}

// 追加で見つけた不整合の回帰テスト: DeleteRule はルール削除時、投資
// （program_intents または program_overrides）のない導出予約だけを物理削除し、
// 投資がある予約は detached 化して残す（docs/recording.md §4.3「ルール自体の
// 削除も同じ規則」）。分離前は program_intents の存在だけを見ていたため、
// 「ルール由来の予約に PATCH しただけ（program_overrides のみ、program_intents
// には行なし）」という M2-4 で新たに可能になった状態のとき、投資を見落として
// 物理削除してしまう欠陥があった。
func TestDeleteRule_ReservationWithOnlyOverridesSurvivesDetached(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1950000195091234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 19500, 1950)

	// program_overrides のみを作る（program_intents には触れない）。
	resp := doPatch(t, srv, "/api/reservations/"+itoa(resID), `{"priority":9}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seeding patch status = %d, want 200", resp.StatusCode)
	}
	_ = decodeReservation(t, resp)
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Fatalf("test setup: program_intents should not exist, err=%v", err)
	}

	delResp := doDelete(t, srv, "/api/rules/"+itoa(ruleID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete rule status = %d, want 200", delResp.StatusCode)
	}
	_ = delResp.Body.Close()

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE id = $1`, resID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reservation row count = %d, want 1 "+
			"(a reservation with only program_overrides must survive rule deletion as detached, not be deleted)", n)
	}
}

func errIsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func jsonMap(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshalling %s: %v", raw, err)
	}
	return m
}

// intJSONNumber は json.Decode で any に入った数値（float64）を int に変換する。
func intJSONNumber(t *testing.T, v interface{}) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("value %#v is not a JSON number", v)
	}
	return int(f)
}
