package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

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
// 使う --- 'GR' だと、`ReservationChannelTypeGR` を決め打ちで返す変異や、
// 同じテスト内の他のフィクスチャ（'GR' 固定）の値を取り違えて読む変異を
// 見逃す。'BS' はこのテストでしか使わない値なので、そうした変異は確実に
// 失敗する。
func TestReservation_ChannelTypeExposed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	srv := api.NewTestServer(t, pool)

	const programID int64 = 1150000115041299
	insertReservationWithChannelType(t, pool, ctx, programID, "BS")

	t.Run("(site, programId) 読み取り", func(t *testing.T) {
		got := api.GetJSON[api.Reservation](t, srv.URL+reservationPath(programID))
		if got.ChannelType != "BS" {
			t.Errorf("channelType = %q, want %q（未設定なら空文字が返る）", got.ChannelType, "BS")
		}
	})

	t.Run("一覧", func(t *testing.T) {
		got := api.GetJSON[[]api.Reservation](t, srv.URL+"/api/reservations")
		if len(got) != 1 {
			t.Fatalf("len(reservations) = %d, want 1", len(got))
		}
		if got[0].ChannelType != "BS" {
			t.Errorf("channelType = %q, want %q（未設定なら空文字が返る）", got[0].ChannelType, "BS")
		}
	})
}
