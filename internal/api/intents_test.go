package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 取消（PUT .../intent {action: skip}）は program_intents.action を倒すだけで
// program_overrides には一切触れない（別軸。issue #29 / docs/recording.md §4.4）。
// api は reservations に一切触れない —— 導出行の削除は ruler の次パスに委ねる
// （issue #29 の決定: reservations の書き手は ruler だけにする）。
func TestPutProgramIntent_SkipDoesNotTouchOverrides(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	srv := api.NewTestServer(t, pool)

	const programID int64 = 327360102415397
	// intent / overrides はいずれも EPG プロジェクションからチャンネル識別情報を
	// 引くように なったので、番組がプロジェクションに乗っていないと 400 になる。
	// ここでは programID=327360102415397（networkID=32736, serviceID=1024,
	// eventID=15397。internal/mirakc/ids_test.go の実測値と同じ）に対応する行を
	// 用意する。
	insertProgramFixture(t, pool, ctx, programID, 32736, 1024)

	putIntent := func(action string) int {
		body := `{"action":"` + action + `"}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPut,
			srv.URL+"/api/sites/default/programs/"+itoa(programID)+"/intent", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if status := putIntent("record"); status != http.StatusNoContent {
		t.Fatalf("put intent{record} status = %d, want 204", status)
	}

	patchBody := `{"priority":7}`
	patchReq, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		srv.URL+"/api/sites/default/programs/"+itoa(programID)+"/overrides", strings.NewReader(patchBody))
	if err != nil {
		t.Fatal(err)
	}
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = patchResp.Body.Close() }()
	if patchResp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch overrides status = %d, want 204", patchResp.StatusCode)
	}

	detail, err := pgstoretest.EPG(pool).GetProgram(ctx, "default", programID)
	if err != nil {
		t.Fatalf("reading program after record: %v", err)
	}
	if detail.IntentAction == nil {
		t.Fatal("intent after record: no row")
	}
	if *detail.IntentAction != "record" {
		t.Errorf("action after record = %q, want record", *detail.IntentAction)
	}
	overrides, found := dbtest.ReadProgramOverrides(t, pool, "default", programID)
	if !found {
		t.Fatal("overrides after patch: no row")
	}
	if got := overridesPriority(t, overrides.Overrides); got != 7 {
		t.Errorf("overrides priority = %d, want 7 (%s)", got, overrides.Overrides)
	}

	// 取消
	if status := putIntent("skip"); status != http.StatusNoContent {
		t.Fatalf("put intent{skip} status = %d, want 204", status)
	}

	// api は reservations に一切触れない。導出行の削除は ruler の次パスに委ねる。
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reservation rows after skip = %d, want 0 (api never writes reservations)", n)
	}

	// 意図は skip として残る。これがないと ruler が復活させてしまう
	detail, err = pgstoretest.EPG(pool).GetProgram(ctx, "default", programID)
	if err != nil {
		t.Fatalf("reading program after skip: %v", err)
	}
	if detail.IntentAction == nil {
		t.Fatal("intent after skip: no row (skip intent must survive)")
	}
	if *detail.IntentAction != "skip" {
		t.Errorf("action after skip = %q, want skip", *detail.IntentAction)
	}
	// 取消は program_intents.action を倒すだけで、program_overrides には
	// 一切触れない（別の軸なので、ユーザーが設定した上書きは保たれる）。
	overrides, found = dbtest.ReadProgramOverrides(t, pool, "default", programID)
	if !found {
		t.Fatal("overrides after skip: no row (PutProgramIntent must not touch program_overrides)")
	}
	if got := overridesPriority(t, overrides.Overrides); got != 7 {
		t.Errorf("overrides lost on cancel: priority = %d, want 7 (%s)", got, overrides.Overrides)
	}
}

// GC は意図の寿命を放送の寿命に揃える。#27 で GC は
// DeleteEndedProgramSnapshots 1 本に集約された（旧 DeleteEndedProgramIntents は
// 撤去済み）。program_intents は program_snapshots への FK が ON DELETE CASCADE
// なので、番組終了 + 猶予経過の program_snapshots 行が消えれば一緒に落ちる。
func TestDeleteEndedProgramIntents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	past := time.Now().Add(-3 * time.Hour)
	future := time.Now().Add(3 * time.Hour)
	for i, st := range []time.Time{past, future} {
		programID := int64(9000 + i)
		dbtest.ProgramSnapshot(t, pool, programID, func(p *dbtest.ProgramSnapshotParams) {
			p.StartAt = st
		})
		dbtest.ProgramIntent(t, pool, programID, func(p *dbtest.ProgramIntentParams) {
			p.Action = "skip"
		})
	}

	deleted, err := pgstoretest.Reservations(pool).PruneEndedSnapshots(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted program_snapshots = %d, want 1 (only the ended program)", deleted)
	}
	if _, ok := dbtest.ReadProgramIntent(t, pool, "default", 9001); !ok {
		t.Error("future intent should survive GC")
	}
	if _, ok := dbtest.ReadProgramIntent(t, pool, "default", 9000); ok {
		t.Error("ended intent should be gone via FK CASCADE from program_snapshots")
	}
}

// overridesPriority は program_overrides.overrides の priority を取り出す。
// jsonb は Postgres 側で正規化されるので文字列比較はしない。
func overridesPriority(t *testing.T, raw []byte) int {
	t.Helper()
	var m struct {
		Priority *int `json:"priority"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshalling overrides %s: %v", raw, err)
	}
	if m.Priority == nil {
		return 0
	}
	return *m.Priority
}

// insertProgramFixture は EPG プロジェクションに、指定した programId に対応する
// サービス・番組の行を用意する。CreateReservation は GetProgramSnapshotSource で
// このプロジェクションを引くので、テストで手動予約を作るにはこれが必要。
func insertProgramFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, networkID, serviceID int) {
	t.Helper()

	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ('default', $1, $2, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`,
		networkID, serviceID); err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}

	start := time.Now().Add(24 * time.Hour)
	end := start.Add(30 * time.Minute)
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description
) VALUES ('default', $1::bigint, $2, $3, 0, $4::timestamptz, 1800000, $5::timestamptz, true, 'テスト番組', '')
ON CONFLICT (site, program_id) DO NOTHING`,
		programID, networkID, serviceID, start, end); err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
