package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
)

// reservationSiteResp は site フィールドだけを見るデコード用型。
type reservationSiteResp struct {
	Id   int64  `json:"id"`
	Site string `json:"site"`
}

// TestReservation_SiteExposed は API が予約の site を返すことを確認する。
//
// 容量超過の判定はサイトごとに独立している（docs/data.md §6.5）ため、予約一覧の
// バッジは予約の site と超過区間の site を突き合わせる必要がある。site を返さないと
// クライアントが単一サイト前提の定数を持つことになり、多サイト化のときに
// 「他サイトの不足を自分の予約の不足として出す」形で静かに壊れる。
//
// **空文字が返らないことを見るのが要点。** site は openapi で required だが Go は
// 構造体の未設定フィールドをエラーにしないので、`reservationFromRow` が
// `Site: r.Site` を落としてもコンパイルは通り、`""` が返るだけになる。
// 一覧・単体の両経路を見る（片方だけ埋めた実装で通らないようにする）。
func TestReservation_SiteExposed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115041234
	resID := insertReservationDirect(t, pool, ctx, programID, nil, 11500, 1150)

	t.Run("単体取得", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/reservations/" + itoa(resID))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got reservationSiteResp
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if got.Site != db.DefaultSite {
			t.Errorf("site = %q, want %q（未設定なら空文字が返る）", got.Site, db.DefaultSite)
		}
	})

	t.Run("一覧", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/reservations")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got []reservationSiteResp
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(reservations) = %d, want 1", len(got))
		}
		if got[0].Site != db.DefaultSite {
			t.Errorf("site = %q, want %q（未設定なら空文字が返る）", got[0].Site, db.DefaultSite)
		}
	})
}
