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

// insertRuleFixture は最小構成のルールを 1 件作る。
func insertRuleFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO rules (name) VALUES ('テストルール') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	return id
}

// insertReservationDirect は program_snapshots + reservations 行を直接作る
// （ruler を経由しない）。ruleID の有無で「ルール由来」「手動」を模した行を
// 作るが、program_intents には触れない（reservations.source 列は issue #26 で
// 削除済み。この直生成では intent が無いため、reservationFromRow 経由の
// API 表示は常に rule になる）。overrides API は (site, programId) を宛先に
// program_overrides だけを書くので（issue #29）、この関数はもっぱら
// 「ルール削除時に投資のある予約が detached で残るか」等、reservations 行
// そのものの生死を確認するテストのための下準備として使う。
//
// #27 で番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）が
// program_snapshots に抽出され、reservations への FK が張られたため、
// 予約行より先に program_snapshots を作る。
func insertReservationDirect(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, ruleID *int64, networkID, serviceID int32) int64 {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ('default', $1, 'テスト番組', $2, 1800000, $3, $4, 'GR', '27', $5, 'テスト局')`,
		programID, start, networkID, serviceID, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
	var id int64
	err := pool.QueryRow(ctx, `
INSERT INTO reservations (site, program_id, rule_id, base)
VALUES ('default', $1, $2, '{}'::jsonb)
RETURNING id`, programID, ruleID).Scan(&id)
	if err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
	return id
}

func overridesPath(programID int64) string {
	return "/api/sites/default/programs/" + itoa(programID) + "/overrides"
}

// reservationPath は (site, programId) を宛先にした予約単体取得の URL
// （GET /api/sites/{site}/programs/{programId}/reservation、issue #99）。
// 旧 GET /api/reservations/{id} は不安定な reservations.id を宛先にしていたため
// 廃止された（issue #440）。テストの読み取りプローブはこちらに寄せる。
func reservationPath(programID int64) string {
	return "/api/sites/default/programs/" + itoa(programID) + "/reservation"
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

// 1. ルール由来の予約に PATCH すると program_overrides に行が新規作成される。
// **program_intents には行ができない**（これが分離の核心。1 表だった頃は
// action='record' が立っていた。docs/recording.md §4.2）。
func TestPatchProgramOverrides_CreatesOverrideForRuleReservation_NotIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1100000110011234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 11000, 1100)

	// 事前に意図も上書きも存在しない。
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Fatalf("intent should not exist before patch, got err=%v", err)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Fatalf("overrides should not exist before patch, got err=%v", err)
	}

	resp := doPatch(t, srv, overridesPath(programID), `{"priority":7}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
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
func TestDeleteProgramOverrides_DoesNotTouchProgramIntents_ManualReservationSurvives(t *testing.T) {
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
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentRecord,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID, Overrides: []byte(`{"priority":5}`),
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doDelete(t, srv, overridesPath(programID))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete overrides status = %d, want 204", resp.StatusCode)
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
func TestPatchProgramOverrides_EmptyPatch_DoesNotTouchProgramIntents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 手動予約 + ルールが後からマッチした状況（rule_id 付き）を再現する。
	const programID int64 = 1160000116011234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 11600, 1160)
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentRecord,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}

	for _, body := range []string{`{}`, `{"reset":[]}`} {
		t.Run(body, func(t *testing.T) {
			resp := doPatch(t, srv, overridesPath(programID), body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("patch %s status = %d, want 204 (empty patch is a harmless no-op)", body, resp.StatusCode)
			}

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
func TestPatchProgramOverrides_PartialUpdatePreservesOtherFields(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1200000120021234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 12000, 1200)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides: []byte(`{"priority":5,"keepOriginal":"always"}`),
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doPatch(t, srv, overridesPath(programID), `{"encodeProfiles":["h264"]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
	}

	overrides, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID})
	if err != nil {
		t.Fatalf("overrides after patch: %v", err)
	}
	got := jsonMap(t, overrides.Overrides)
	if intJSONNumber(t, got["priority"]) != 5 {
		t.Errorf("priority should be preserved, got %+v", got)
	}
	if got["keepOriginal"] != "always" {
		t.Errorf("keepOriginal should be preserved, got %+v", got)
	}
	profiles, ok := got["encodeProfiles"].([]interface{})
	if !ok || len(profiles) != 1 || profiles[0] != "h264" {
		t.Errorf("encodeProfiles should be set, got %+v", got)
	}
}

// 8. reset でフィールド単位に override が消え、他は残る。
func TestPatchProgramOverrides_ResetRemovesOnlyThatField(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1300000130031234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 13000, 1300)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides: []byte(`{"priority":5,"keepOriginal":"always"}`),
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doPatch(t, srv, overridesPath(programID), `{"reset":["priority"]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
	}

	overrides, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID})
	if err != nil {
		t.Fatalf("overrides row should still exist (not empty): %v", err)
	}
	got := jsonMap(t, overrides.Overrides)
	if _, ok := got["priority"]; ok {
		t.Errorf("priority should be reset away, got %+v", got)
	}
	if got["keepOriginal"] != "always" {
		t.Errorf("keepOriginal should be preserved, got %+v", got)
	}
}

// 9. reset で最後の override が消えたら program_overrides の行自体が消える。
func TestPatchProgramOverrides_ResetLastField_DeletesOverridesRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1400000140041234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 14000, 1400)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides: []byte(`{"priority":5}`),
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doPatch(t, srv, overridesPath(programID), `{"reset":["priority"]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
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
func TestDeleteProgramOverrides_ClearsAll(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1600000160061234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 16000, 1600)
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides: []byte(`{"priority":5,"keepOriginal":"always"}`),
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	resp := doDelete(t, srv, overridesPath(programID))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete overrides status = %d, want 204", resp.StatusCode)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("overrides row should be deleted, err=%v", err)
	}
}

// 11. 400 群: 値と reset の衝突 / 未知の reset フィールド名 / 不正な keepOriginal /
// 不正な filenameTemplate / 負の priority。
func TestPatchProgramOverrides_ValueAndResetConflict_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1700000170071234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 17000, 1700)

	resp := doPatch(t, srv, overridesPath(programID), `{"priority":5,"reset":["priority"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID}); !errIsNoRows(err) {
		t.Errorf("no overrides should be created on a rejected request, err=%v", err)
	}
}

func TestPatchProgramOverrides_UnknownResetField_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1800000180081234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 18000, 1800)

	resp := doPatch(t, srv, overridesPath(programID), `{"reset":["skip"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (skip is not an overrides key)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp2 := doPatch(t, srv, overridesPath(programID), `{"reset":["totallyUnknownField"]}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (typo'd field name)", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}

// config に無い encodeProfiles は overrides でも 400 にする（issue #64）。
// 既知名が通ることも両方向で確認する。
func TestPatchProgramOverrides_UnknownEncodeProfile(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1850000185081234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 18500, 1850)

	resp := doPatch(t, srv, overridesPath(programID), `{"encodeProfiles":["no-such-profile"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown encode profile status = %d, want 400", resp.StatusCode)
	}
	var errBody api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(errBody.Error, "unknown encode profile") {
		t.Errorf("error body = %q, want mention of unknown encode profile", errBody.Error)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site: "default", ProgramID: programID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("rejected request created program_overrides, err=%v", err)
	}

	resp = doPatch(t, srv, overridesPath(programID), `{"encodeProfiles":["h264"]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("known encode profile status = %d, want 204", resp.StatusCode)
	}
	row, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("loading known-profile overrides: %v", err)
	}
	var stored struct {
		EncodeProfiles []string `json:"encodeProfiles"`
	}
	if err := json.Unmarshal(row.Overrides, &stored); err != nil {
		t.Fatalf("unmarshalling stored overrides %s: %v", row.Overrides, err)
	}
	if len(stored.EncodeProfiles) != 1 || stored.EncodeProfiles[0] != "h264" {
		t.Errorf("stored encodeProfiles = %v, want [h264]", stored.EncodeProfiles)
	}
}

func TestPatchProgramOverrides_InvalidFields_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1900000190091234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 19000, 1900)

	resp := doPatch(t, srv, overridesPath(programID), `{"keepOriginal":"sometimes"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid keepOriginal: status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp2 := doPatch(t, srv, overridesPath(programID), `{"filenameTemplate":"{{.NoSuchField}}"}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid filenameTemplate: status = %d, want 400", resp2.StatusCode)
	}
	_ = resp2.Body.Close()

	resp3 := doPatch(t, srv, overridesPath(programID), `{"priority":-1}`)
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative priority: status = %d, want 400", resp3.StatusCode)
	}
	_ = resp3.Body.Close()

	// 空文字の contentPath は保存が成功するが reconciler の差分対象から外れて
	// 何も反映されない状態になる（explicitContentPath、internal/reconciler）ため、
	// 保存時点で拒否する（issue #312）。override を消すのは reset。
	resp4 := doPatch(t, srv, overridesPath(programID), `{"contentPath":""}`)
	if resp4.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty contentPath: status = %d, want 400", resp4.StatusCode)
	}
	_ = resp4.Body.Close()
}

// insertProgramSnapshotOnly は program_snapshots だけを作る（reservations 行は
// 作らない）。overrides API が (site, programId) を自身の宛先に持ち、既存
// schedule（導出射影である schedule_sync）の有無に依存しないことの確認に使う
// （issue #312 の方針決定 (b): B 案（schedule の有無で拒否）を採らなかったこと
// の明文化）。
func insertProgramSnapshotOnly(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, networkID, serviceID int32) {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ('default', $1, 'テスト番組', $2, 1800000, $3, $4, 'GR', '27', $5, 'テスト局')`,
		programID, start, networkID, serviceID, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
}

// schedule どころか reservations 行すら無い（EPG 射影にだけ番組がある）予約に
// contentPath を明示指定しても 200 系で成功すること。B 案（schedule_sync の有無で
// 拒否する）を採らなかったことの直接の確認 — 拒否条件を導出射影に依存させると
// 「同じ PATCH が reconcile パスとの競走で結果が時刻で変わる」契約劣化が起きる
// （issue #312 の方針決定）。
func TestPatchProgramOverrides_ContentPathWithoutExistingSchedule_Succeeds(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1900000190099999
	insertProgramSnapshotOnly(t, pool, ctx, programID, 19000, 1900)

	resp := doPatch(t, srv, overridesPath(programID), `{"contentPath":"custom/x"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("contentPath patch with no existing reservation/schedule: status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	q := sqlcgen.New(pool)
	row, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: programID})
	if err != nil {
		t.Fatalf("getting program overrides: %v", err)
	}
	var stored struct {
		ContentPath *string `json:"contentPath"`
	}
	if err := json.Unmarshal(row.Overrides, &stored); err != nil {
		t.Fatalf("unmarshalling overrides: %v", err)
	}
	if stored.ContentPath == nil || *stored.ContentPath != "custom/x" {
		t.Errorf("stored contentPath = %v, want %q", stored.ContentPath, "custom/x")
	}
}

// 12. programId が EPG プロジェクションに無い PATCH は 400（旧: 存在しない予約への
// PATCH/DELETE は 404 だったが、宛先が (site, programId) になったことで
// 「予約の有無」という概念が無くなった。issue #29）。
// DELETE は行の有無を問わず冪等に 204 を返す（DeleteRecording と同じ規律）。
func TestPatchProgramOverrides_ProgramNotInProjection_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const missingProgramID = 999999

	resp := doPatch(t, srv, overridesPath(missingProgramID), `{"priority":5}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("patch for program not in EPG projection: status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestDeleteProgramOverrides_NoRow_IsIdempotent(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const missingProgramID = 999999

	resp := doDelete(t, srv, overridesPath(missingProgramID))
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete overrides with no row: status = %d, want 204 (idempotent)", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// 13. PATCH の結果が db.EffectiveOptions を通して期待どおりの effective になる。
func TestPatchProgramOverrides_EffectiveOptionsRoundTrip(t *testing.T) {
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

	resp := doPatch(t, srv, overridesPath(programID), `{"priority":7}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	row, err := q.GetReservationFullBySiteAndProgramID(ctx, sqlcgen.GetReservationFullBySiteAndProgramIDParams{
		Site: "default", ProgramID: programID,
	})
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

// 14. keepOriginal/encodeProfiles の実効値検証（issue #104）。
//
// バグの再現条件は「プロファイルを持たないルール由来の予約、または手動予約」に
// keepOriginal=until_encoded だけを立てること。insertReservationDirect は
// base='{}'（プロファイル無し）で作るため、この 2 テストはどちらもそのまま
// 再現条件を満たす。リクエスト単体では見えない組み合わせ（reset で
// encodeProfiles を消す経路）も同じく弾かれることを確認する。
func TestPatchProgramOverrides_UntilEncodedWithoutProfiles_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 2200000220121234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 22000, 2200)
	// base はプロファイル無し（insertReservationDirect の既定 '{}'）。

	resp := doPatch(t, srv, overridesPath(programID), `{"keepOriginal":"until_encoded"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("keepOriginal alone with no profiles anywhere: status = %d, want 400", resp.StatusCode)
	}
	var errBody api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBody.Error, "encodeProfiles") {
		t.Errorf("error body = %q, want mention of encodeProfiles", errBody.Error)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site: "default", ProgramID: programID,
	}); !errIsNoRows(err) {
		t.Errorf("rejected request created program_overrides, err=%v", err)
	}
}

func TestPatchProgramOverrides_UntilEncodedResetEncodeProfiles_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 2300000230131234
	ruleID := insertRuleFixture(t, pool, ctx)
	insertReservationDirect(t, pool, ctx, programID, &ruleID, 23000, 2300)
	// 既存の override に encodeProfiles を立てておく。単体のリクエストボディだけ
	// 見ると reset:["encodeProfiles"] は encodeProfiles に触れていないように
	// 見えるが、マージ後の実効値では消える。
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID,
		Overrides: []byte(`{"encodeProfiles":["h264"]}`),
	}); err != nil {
		t.Fatalf("seeding existing overrides: %v", err)
	}

	resp := doPatch(t, srv, overridesPath(programID),
		`{"keepOriginal":"until_encoded","reset":["encodeProfiles"]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("keepOriginal + reset encodeProfiles: status = %d, want 400", resp.StatusCode)
	}

	// 拒否されたリクエストは既存の override を変えない。
	row, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("existing overrides should survive a rejected patch, err=%v", err)
	}
	if !strings.Contains(string(row.Overrides), "h264") {
		t.Errorf("existing overrides changed: %s", row.Overrides)
	}
}

// 反対方向: base がプロファイルを持っていれば、override 側は keepOriginal
// だけを立てても通る（base を通じて実効値が満たされる）。
func TestPatchProgramOverrides_UntilEncodedWithProfilesFromBase_Succeeds(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 2400000240141234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 24000, 2400)
	if _, err := pool.Exec(ctx, `UPDATE reservations SET base = $1 WHERE id = $2`,
		[]byte(`{"encodeProfiles":["h264"]}`), resID); err != nil {
		t.Fatalf("seeding base with profiles: %v", err)
	}

	resp := doPatch(t, srv, overridesPath(programID), `{"keepOriginal":"until_encoded"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("keepOriginal alone with profiles from base: status = %d, want 204", resp.StatusCode)
	}

	row, err := q.GetReservationFullBySiteAndProgramID(ctx, sqlcgen.GetReservationFullBySiteAndProgramIDParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("reloading reservation: %v", err)
	}
	eff, err := db.EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
	if err != nil {
		t.Fatalf("computing effective options: %v", err)
	}
	if eff.KeepOriginal == nil || *eff.KeepOriginal != db.KeepOriginalUntilEncoded {
		t.Errorf("effective keepOriginal = %v, want %q", eff.KeepOriginal, db.KeepOriginalUntilEncoded)
	}
	if eff.EncodeProfiles == nil || len(*eff.EncodeProfiles) != 1 || (*eff.EncodeProfiles)[0] != "h264" {
		t.Errorf("effective encodeProfiles = %v, want [h264] (from base)", eff.EncodeProfiles)
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
	resp := doPatch(t, srv, overridesPath(programID), `{"priority":9}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("seeding patch status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()
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

// 回帰テスト（#162）: ルール削除時の投資判定を program_investments view
// （program_intents の action='record' 行 ∪ program_overrides の行）に揃えた。
// 揃える前は program_intents の存在だけを見ており action を限定しなかったため、
// intent{skip} だけの予約（そもそも desired に入らない = ruler.sql の導出削除
// ガードと同じ理由づけ）が detached として残ると数えられてしまい、直後の
// ruler パスで導出削除される数秒だけの行を「detached になった」と数える
// 不整合があった。揃えた後は削除側に数えられ、行も同一トランザクションで消える。
func TestDeleteRule_ReservationWithOnlySkipIntentIsDeleted(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1960000196091234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 19600, 1960)

	// program_intents に action='skip' だけを作る（program_overrides には触れない）。
	if _, err := pool.Exec(ctx, `
INSERT INTO program_intents (site, program_id, action) VALUES ('default', $1, 'skip')`,
		programID); err != nil {
		t.Fatalf("seeding skip intent: %v", err)
	}

	delResp := doDelete(t, srv, "/api/rules/"+itoa(ruleID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete rule status = %d, want 200", delResp.StatusCode)
	}
	var del api.DeleteRuleResponse
	if err := json.NewDecoder(delResp.Body).Decode(&del); err != nil {
		t.Fatal(err)
	}
	_ = delResp.Body.Close()
	if del.DeletedReservations != 1 || del.DetachedReservations != 0 {
		t.Errorf("deletedReservations=%d detachedReservations=%d, want 1/0 "+
			"(a reservation with only an intent{skip} row is not a user investment "+
			"and must be physically deleted, not detached)",
			del.DeletedReservations, del.DetachedReservations)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE id = $1`, resID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reservation row count = %d, want 0 "+
			"(a reservation with only an intent{skip} row must be deleted alongside the rule, "+
			"not kept as detached)", n)
	}
}

// 反対方向（#162）: program_intents に action='record' がある予約は
// program_investments に含まれるので、ルール削除後も detached で残る。
func TestDeleteRule_ReservationWithOnlyRecordIntentSurvivesDetached(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1970000197091234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 19700, 1970)

	if _, err := pool.Exec(ctx, `
INSERT INTO program_intents (site, program_id, action) VALUES ('default', $1, 'record')`,
		programID); err != nil {
		t.Fatalf("seeding record intent: %v", err)
	}

	delResp := doDelete(t, srv, "/api/rules/"+itoa(ruleID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete rule status = %d, want 200", delResp.StatusCode)
	}
	var del api.DeleteRuleResponse
	if err := json.NewDecoder(delResp.Body).Decode(&del); err != nil {
		t.Fatal(err)
	}
	_ = delResp.Body.Close()
	if del.DeletedReservations != 0 || del.DetachedReservations != 1 {
		t.Errorf("deletedReservations=%d detachedReservations=%d, want 0/1 "+
			"(a reservation with an intent{record} row is a user investment and "+
			"must survive rule deletion as detached)",
			del.DeletedReservations, del.DetachedReservations)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE id = $1`, resID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reservation row count = %d, want 1 "+
			"(a reservation with an intent{record} row must survive rule deletion, not be deleted)", n)
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
