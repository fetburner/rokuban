package api_test

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

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

	srv := api.NewTestServer(t, pool)

	const programID int64 = 1150000115041234
	insertReservationDirect(t, pool, ctx, programID, nil, 11500, 1150)

	t.Run("単体取得", func(t *testing.T) {
		got := api.GetJSON[api.Reservation](t, srv.URL+reservationPath(programID))
		if got.Site != store.DefaultSite {
			t.Errorf("site = %q, want %q（未設定なら空文字が返る）", got.Site, store.DefaultSite)
		}
	})

	t.Run("一覧", func(t *testing.T) {
		got := api.GetJSON[[]api.Reservation](t, srv.URL+"/api/reservations")
		if len(got) != 1 {
			t.Fatalf("len(reservations) = %d, want 1", len(got))
		}
		if got[0].Site != store.DefaultSite {
			t.Errorf("site = %q, want %q（未設定なら空文字が返る）", got[0].Site, store.DefaultSite)
		}
	})
}
