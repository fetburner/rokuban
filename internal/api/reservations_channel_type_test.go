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

// reservationChannelTypeResp は channelType だけを見るデコード用型。
type reservationChannelTypeResp struct {
	Id          int64  `json:"id"`
	ChannelType string `json:"channelType"`
}

// insertReservationWithChannelType は program_snapshots.channel_type を指定して
// program_snapshots + reservations 行を直接作る（ruler を経由しない）。
func insertReservationWithChannelType(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, channelType string) {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ('default', $1, 'テスト番組', $2, 1800000, 11500, 1150, $3, '27', $4, 'テスト局')`,
		programID, start, channelType, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base)
VALUES ('default', $1, '{}'::jsonb)`, programID); err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
}

// TestReservation_ChannelTypeExposed は issue #440 の受け入れ基準: API が予約に
// program_snapshots.channel_type 由来の channelType を返すことを確認する
// （ライブ画面が EPG 経由の programId 突き合わせなしに「同じチャンネル種別の
// 予約か」を判定できるようにするため。web/src/lib/live-interruption.ts）。
//
// 単体（(site, programId) キー）・一覧の両経路を見る（reservationFromRow が
// 1 箇所に集約されていることの確認。site / serviceName の既存テストと同じ
// 流儀）。既定値として他所のフィクスチャで使われがちな 'GR' ではなく 'BS' を
// 使う --- 'GR' だと reservationFromRow が ChannelType を落として空文字を
// 返してもテストの期待値と一致しない保証がないため、ゼロ値と紛れない値を選ぶ。
func TestReservation_ChannelTypeExposed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115041299
	insertReservationWithChannelType(t, pool, ctx, programID, "BS")

	t.Run("(site, programId) 読み取り", func(t *testing.T) {
		resp, err := http.Get(srv.URL + reservationPath(programID))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got reservationChannelTypeResp
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if got.ChannelType != "BS" {
			t.Errorf("channelType = %q, want %q（未設定なら空文字が返る）", got.ChannelType, "BS")
		}
	})

	t.Run("一覧", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/reservations")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got []reservationChannelTypeResp
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(reservations) = %d, want 1", len(got))
		}
		if got[0].ChannelType != "BS" {
			t.Errorf("channelType = %q, want %q（未設定なら空文字が返る）", got[0].ChannelType, "BS")
		}
	})
}
