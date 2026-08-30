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
	"github.com/fetburner/rokuban/internal/testutil"
)

// reservationServiceNameResp は title / serviceName だけを見るデコード用型。
type reservationServiceNameResp struct {
	Id          int64  `json:"id"`
	ProgramId   int64  `json:"programId"`
	Title       string `json:"title"`
	ServiceName string `json:"serviceName"`
}

// insertReservationWithSnapshot は program_snapshots + reservations を直接
// 作る（ruler を経由しない）。title / serviceName を呼び出し側で指定できる点が
// insertReservationDirect（reservations_overrides_test.go）と異なる ---
// 「同タイトル別局」の再現には、両者ともハードコードされた 'テスト番組' /
// 'テスト局' を返す既存ヘルパーでは区別できないため。
func insertReservationWithSnapshot(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	programID int64,
	title, serviceName string,
	networkID, serviceID int32,
) {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ('default', $1, $2, $3, 1800000, $4, $5, 'GR', '27', $6, $7)`,
		programID, title, start, networkID, serviceID, int32(programID%100000), serviceName); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base)
VALUES ('default', $1, '{}'::jsonb)`, programID); err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
}

// TestReservation_ServiceNameExposed は API が予約に program_snapshots.service_name
// 由来のチャンネル表示名を返すことを確認する（issue #302）。
//
// 予約一覧・ホーム・予約詳細はいずれも reservationFromRow を経由するため
// （internal/api/handler.go / program_reservation.go）、単体取得・一覧の両経路を
// 見れば全画面の配線を代表できる。site 同様、openapi では required だが Go は
// 未設定フィールドをエラーにしないので、`reservationFromRow` が
// `ServiceName: snap.ServiceName` を落としてもコンパイルは通り空文字が返るだけ
// になる --- 空文字が返らないことを見るのが要点。
func TestReservation_ServiceNameExposed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115041234
	insertReservationWithSnapshot(t, pool, ctx, programID, "テスト番組", "NHK総合", 1, 1)

	t.Run("単体取得", func(t *testing.T) {
		resp, err := http.Get(srv.URL + reservationPath(programID))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got reservationServiceNameResp
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if got.ServiceName != "NHK総合" {
			t.Errorf("serviceName = %q, want %q（未設定なら空文字が返る）", got.ServiceName, "NHK総合")
		}
	})

	t.Run("一覧", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/reservations")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got []reservationServiceNameResp
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(reservations) = %d, want 1", len(got))
		}
		if got[0].ServiceName != "NHK総合" {
			t.Errorf("serviceName = %q, want %q（未設定なら空文字が返る）", got[0].ServiceName, "NHK総合")
		}
	})
}

// TestReservation_ServiceNameDistinguishesSameTitle は issue #302 の受け入れ
// 基準そのもの --- 同一タイトルで局違いの 2 件を並べたとき、serviceName だけで
// 区別できることを確認する。programId で行を引き、それぞれの serviceName が
// 期待どおりであることを見る（タイトルが同一なので title では区別できない）。
func TestReservation_ServiceNameDistinguishesSameTitle(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const title = "ゆう6かがわ▽県内ニュース・気象　ほか"
	const programA int64 = 1150000115041001
	const programB int64 = 1150000115041002
	insertReservationWithSnapshot(t, pool, ctx, programA, title, "NHK総合・高松", 1, 101)
	insertReservationWithSnapshot(t, pool, ctx, programB, title, "NHK Eテレ・高松", 1, 102)

	resp, err := http.Get(srv.URL + "/api/reservations")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got []reservationServiceNameResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(reservations) = %d, want 2", len(got))
	}

	byProgram := make(map[int64]reservationServiceNameResp, 2)
	for _, r := range got {
		if r.Title != title {
			t.Errorf("programId %d: title = %q, want %q", r.ProgramId, r.Title, title)
		}
		byProgram[r.ProgramId] = r
	}
	if got := byProgram[programA].ServiceName; got != "NHK総合・高松" {
		t.Errorf("programId %d: serviceName = %q, want %q", programA, got, "NHK総合・高松")
	}
	if got := byProgram[programB].ServiceName; got != "NHK Eテレ・高松" {
		t.Errorf("programId %d: serviceName = %q, want %q", programB, got, "NHK Eテレ・高松")
	}
	if byProgram[programA].ServiceName == byProgram[programB].ServiceName {
		t.Fatal("同タイトルの 2 件が同じ serviceName を返しており、局名で区別できない")
	}
}
