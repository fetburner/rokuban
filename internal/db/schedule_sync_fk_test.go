package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// TestScheduleSyncReservationID_SurvivesReservationRematerialization は issue #99
// の付随決定（00027_schedule_sync_drop_reservation_fk.sql）の回帰テスト。
//
// schedule_sync.reservation_id は元々 reservations(id) への FK で
// ON DELETE SET NULL だった。ruler が予約を導出削除・再実体化する
// （EPG フリッカー・ルール編集。#53 / #98 / #99 が繰り返し踏んでいる同じ形の
// churn）と、この FK が自動で reservation_id を NULL に落とす。
//
// このカラムは現在どのコードからも読まれていない（ListScheduleSyncsBySite は
// どこからも呼ばれておらず、reconciler の「自分の schedule か」の判定は常に
// tags で行う）ため実害は無いが、手動での DB 調査時に「なぜ自分の schedule の
// reservation_id が NULL なのか」という誤読を招く（issue #99 が挙げた症状:
// 観測行が一時的に外部産と同じ見た目になる）。FK を外したことで、
// reservations 側の DELETE がこの列を自動的に変えなくなったことを確認する。
func TestScheduleSyncReservationID_SurvivesReservationRematerialization(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const site = "default"
	const programID int64 = 990000099011234

	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (site, program_id, title, start_at, duration_ms, network_id, service_id, channel_type, channel)
VALUES ($1, $2, 'テスト番組', $3, 1800000, 99000, 9901, 'GR', '27')`,
		site, programID, start); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}

	res1, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: site, ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("creating initial reservation: %v", err)
	}

	// reconciler.observeSchedules が書く形を模す。
	if err := q.UpsertScheduleSync(ctx, sqlcgen.UpsertScheduleSyncParams{
		Site:          site,
		ProgramID:     programID,
		ReservationID: &res1.ID,
		State:         "scheduled",
		Options:       json.RawMessage(`{}`),
		Tags:          []string{"program:990000099011234"},
	}); err != nil {
		t.Fatalf("seeding schedule_sync: %v", err)
	}

	// ruler の導出削除 → 再実体化を模す（同じ番組・新しい id）。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE id = $1`, res1.ID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	res2, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: site, ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("re-materializing reservation: %v", err)
	}
	if res2.ID == res1.ID {
		t.Fatalf("再実体化で id が変わっていない（テストの前提が崩れている）")
	}

	// 核心: reconciler の次パスがまだ来ていない時点でも、schedule_sync の
	// reservation_id は（FK の ON DELETE SET NULL が無いので）NULL に
	// 落ちない。FK が残っていればここで NULL になり、「自分の schedule なのに
	// 外部産に見える」（issue #99 の症状）が再現する。
	var reservationID *int64
	if err := pool.QueryRow(ctx,
		`SELECT reservation_id FROM schedule_sync WHERE site = $1 AND program_id = $2`,
		site, programID).Scan(&reservationID); err != nil {
		t.Fatalf("querying schedule_sync: %v", err)
	}
	if reservationID == nil {
		t.Error("schedule_sync.reservation_id = nil, want non-nil " +
			"(FK の ON DELETE SET NULL を外したので、予約の再実体化で自動的に NULL には落ちないはず)")
	}
}
