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
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 取消は「意図を残して導出行を落とす」。行を消すだけでは「消された行」と
// 「最初から無かった行」が ruler から区別できず、次の全量パスが復活させてしまう
// （issue #18 の案 A / docs/recording.md §4.4）。
func TestDeleteReservation_KeepsSkipIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 327360102415397
	// CreateReservation は EPG プロジェクションからチャンネル識別情報を引くように
	// なったので、番組がプロジェクションに乗っていないと 400 になる。ここでは
	// programID=327360102415397（networkID=32736, serviceID=1024, eventID=15397。
	// internal/mirakc/ids_test.go の実測値と同じ）に対応する行を用意する。
	insertProgramFixture(t, pool, ctx, programID, 32736, 1024)

	body := `{"programId":327360102415397,"priority":7}`

	resp, err := http.Post(srv.URL+"/api/reservations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		Id        int64                  `json:"id"`
		Overrides map[string]interface{} `json:"overrides"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// 作成時点で意図が record として永続化されている
	q := sqlcgen.New(pool)
	intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("intent after create: %v", err)
	}
	if intent.Action != "record" {
		t.Errorf("action after create = %q, want record", intent.Action)
	}
	// overrides は予約行でも program_intents でもなく program_overrides に載る
	// （M2-4 で分離）。
	overrides, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("overrides after create: %v", err)
	}
	if got := overridesPriority(t, overrides.Overrides); got != 7 {
		t.Errorf("overrides priority = %d, want 7 (%s)", got, overrides.Overrides)
	}
	if created.Overrides == nil || created.Overrides["priority"] == nil {
		t.Errorf("API response should surface overrides from program_overrides: %+v", created.Overrides)
	}

	// 取消
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		srv.URL+"/api/reservations/"+itoa(created.Id), nil)
	if err != nil {
		t.Fatal(err)
	}
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = delResp.Body.Close() }()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delResp.StatusCode)
	}

	// 導出行は消える
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reservation rows after delete = %d, want 0", n)
	}

	// 意図は skip として残る。これがないと ruler が復活させてしまう
	intent, err = q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("intent after delete: %v (skip intent must survive)", err)
	}
	if intent.Action != "skip" {
		t.Errorf("action after delete = %q, want skip", intent.Action)
	}
	// 取消は program_intents.action を倒すだけで、program_overrides には
	// 一切触れない（別の軸なので、ユーザーが設定した上書きは保たれる）。
	overrides, err = q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("overrides after delete: %v (DeleteReservation must not touch program_overrides)", err)
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
	q := sqlcgen.New(pool)

	past := time.Now().Add(-3 * time.Hour)
	future := time.Now().Add(3 * time.Hour)
	for i, st := range []time.Time{past, future} {
		programID := int64(9000 + i)
		if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
			Site: "default", ProgramID: programID, Title: "", StartAt: st, DurationMs: 1800000,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
			Site:      "default",
			ProgramID: programID,
			Action:    "skip",
		}); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := q.DeleteEndedProgramSnapshots(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted program_snapshots = %d, want 1 (only the ended program)", deleted)
	}
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{
		Site: "default", ProgramID: 9001,
	}); err != nil {
		t.Errorf("future intent should survive GC: %v", err)
	}
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{
		Site: "default", ProgramID: 9000,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ended intent should be gone via FK CASCADE from program_snapshots, got err=%v", err)
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
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ('default', $1::bigint, $2, $3, 0, $4::timestamptz, 1800000, $5::timestamptz, true, 'テスト番組', '', '{}'::smallint[])
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
