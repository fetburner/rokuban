package reconciler_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/testutil"
)

// never-scheduled の除外条件が予約の再実体化を跨いで効くこと（issue #98）。
//
// reservations.id は ruler の導出削除・再実体化で変わりうる不安定な値
// （#53 が mirakc の tag を program:{programId} に移した理由。#99 も同じ話）で、
// recordings.reservation_id は当時 ON DELETE SET NULL だった（issue #158 で
// 列自体を削除済み）。除外条件を予約 id で引くと、EPG フリッカーやルール編集で
// 予約行が作り直された瞬間に「never-scheduled 行が無い」ことになり、終了済み
// 予約が毎パス desired に戻り続ける（容量需要にも数え続ける）。宛先のキーは
// 放送イベント (site, network_id, service_id, event_id) でなければならない
// （不変条件 9 の identity: 導出器が作るキーを宛先にしない）。
func TestReconciler_NeverScheduledExclusionSurvivesRematerialization(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	q := sqlcgen.New(pool)
	// 終了済みの番組（30 分尺なので 2 時間前開始なら終了済み）。
	startAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, ctx, q, 700001, "終了済み番組", startAt)

	r := reconciler.New("default", mirakc.NewClient(srv.URL, nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}

	// 前提: 欠測行ができ、次パスから desired に現れない。
	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（欠測行で除外されるはず）", len(rows))
	}

	// ruler の導出削除 → 再実体化を模す（同じ番組・新しい id）。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE id = $1`, res.ID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	res2, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: "default", ProgramID: 700001,
	})
	if err != nil {
		t.Fatalf("re-materializing reservation: %v", err)
	}
	if res2.ID == res.ID {
		t.Fatalf("再実体化で id が変わっていない（テストの前提が崩れている）")
	}

	rows, err = q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation (after rematerialization): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("再実体化後の desired = %d, want 0 —— 欠測の除外が予約 id に依存している（放送イベントで引くべき）", len(rows))
	}

	full, err := q.GetReservationFull(ctx, res2.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if !full.NeverRecorded {
		t.Errorf("再実体化後の never_recorded = false, want true —— 表示も予約 id に依存している")
	}
}

// 放送中の mirakc 由来の失敗では「録れなかった」表示にしないこと。欠測は
// never_scheduled_events 表にしか書かれないので、recordings に failed 行が
// あるだけでは never_recorded は立たない（欠測行が無い）。同期除外も欠測表を
// 引くだけなので、この失敗で desired から除外されないことも合わせて確認する
// （mirakc の再スケジュール待ちの再試行経路を壊さない）。
func TestReservation_MidRecordingFailureIsNotOrphanedDisplay(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	// まだ放送中（10 分前開始・30 分尺）。
	res := createReservation(t, ctx, q, 700002, "放送中の番組", time.Now().Add(-10*time.Minute))

	// handleRecordingFailed が作る形の failed 行（never-scheduled マーカーは無い）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO recordings (
			source, site, network_id, service_id, event_id,
			service_name, channel_type, channel, title, is_free,
			program_start_at, program_duration_ms, status, quality_events
		) VALUES ('manual', 'default', 10000, 5000, $1,
			'テスト局', 'GR', '27', '放送中の番組', true,
			now(), 1800000, 'failed',
			'[{"event":"recording.failed","reason":"need-rescheduling"}]'::jsonb)
	`, int32(700002%100000)); err != nil {
		t.Fatalf("inserting mid-recording failure: %v", err)
	}

	full, err := q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— 放送中の失敗で「録れなかった」と表示している（mirakc の再試行待ち）")
	}

	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Reservation.ID == res.ID {
			found = true
		}
	}
	if !found {
		t.Error("放送中の mirakc 由来の失敗で desired から除外された --- " +
			"再試行経路が壊れている（欠測除外が recordings の failed 行まで拾っている疑い）")
	}
}

// 後から本物の record が着地したら「録れなかった」表示が消えること（#59）、
// ただし欠測行は残り、同期除外は外れないこと（issue #318）。
//
// 欠測（never_scheduled_events）は永続の観測で、本物の record が来ても消さない
// （録画は録画、欠測は欠測）。表示用 never_recorded は「欠測行がある AND その
// 放送イベントに recordings 行が無い」で導出するので、本物の record が来た瞬間に
// false になる（#59「録れたのに orphaned のまま」の構造的解消）。一方で同期除外は
// 欠測行の存在だけを見るので、record が来ても desired には戻らない（終了済み
// 予約を再スケジュールしない）。
func TestReservation_RealRecordClearsOrphanedButKeepsMissingEvent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	q := sqlcgen.New(pool)
	res := createReservation(t, ctx, q, 700003, "後から録れた番組", time.Now().Add(-2*time.Hour))

	r := reconciler.New("default", mirakc.NewClient(srv.URL, nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	full, err := q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if !full.NeverRecorded {
		t.Fatalf("precondition: never_recorded = false, want true（欠測行ができているはず）")
	}

	// 前提: 欠測行があるので同期除外も効いている。
	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（欠測行で除外されるはず）", len(rows))
	}

	// 本物の record が同じ放送イベントに着地する（watcher の CreateRecording 相当）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO recordings (
			source, site, network_id, service_id, event_id,
			service_name, channel_type, channel, title, is_free,
			program_start_at, program_duration_ms, status, started_at, ended_at
		) VALUES ('manual', 'default', 10000, 5000, $1,
			'テスト局', 'GR', '27', '後から録れた番組', true,
			now() - interval '2 hours', 1800000, 'finished',
			now() - interval '2 hours', now() - interval '90 minutes')
	`, int32(700003%100000)); err != nil {
		t.Fatalf("inserting real record: %v", err)
	}

	full, err = q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull (after real record): %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— 本物の record が来たのに「録れなかった」と表示し続けている（#59 の再発）")
	}

	// 欠測行は残り、同期除外は変わらない。
	var missingExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM never_scheduled_events WHERE site='default' AND network_id=10000 AND service_id=5000 AND event_id=$1)`,
		int32(700003%100000)).Scan(&missingExists); err != nil {
		t.Fatalf("checking missing event: %v", err)
	}
	if !missingExists {
		t.Error("欠測行が消えた, want 残存（本物の record が来ても欠測は欠測として残す）")
	}
	rows, err = q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation (after real record): %v", err)
	}
	if len(rows) != 0 {
		t.Error("本物の record 着地後の desired = 1, want 0 —— 同期除外が recordings の有無に依存してしまっている（終了済み予約が再スケジュールされる）")
	}
}

// 本物の record をごみ箱に入れても「録れなかった」表示に戻らないこと（issue #318
// で確定した「any 行」導出）。旧実装では本物の record が欠測行を永久に
// supersede していたため、ごみ箱操作後も orphaned に戻らなかった。この意味を
// 保つため、表示用 never_recorded の recordings 照合は live 限定を掛けない。
// 同期除外も欠測行の存在だけを見るので、ごみ箱操作の影響を受けない。
func TestReservation_TrashedRealRecordDoesNotReenterOrphaned(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	q := sqlcgen.New(pool)
	res := createReservation(t, ctx, q, 700004, "録れた後ごみ箱に入れた番組", time.Now().Add(-2*time.Hour))

	r := reconciler.New("default", mirakc.NewClient(srv.URL, nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	// 本物の record が着地。
	if _, err := pool.Exec(ctx, `
		INSERT INTO recordings (
			source, site, network_id, service_id, event_id,
			service_name, channel_type, channel, title, is_free,
			program_start_at, program_duration_ms, status, started_at, ended_at
		) VALUES ('manual', 'default', 10000, 5000, $1,
			'テスト局', 'GR', '27', '録れた後ごみ箱に入れた番組', true,
			now() - interval '2 hours', 1800000, 'finished',
			now() - interval '2 hours', now() - interval '90 minutes')
	`, int32(700004%100000)); err != nil {
		t.Fatalf("inserting real record: %v", err)
	}

	full, err := q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if full.NeverRecorded {
		t.Fatalf("precondition: never_recorded = true, want false（本物の record で消えているはず）")
	}

	// 本物の record をごみ箱に入れる（SoftDeleteRecording 相当）。
	if _, err := pool.Exec(ctx, `
		UPDATE recordings SET deleted_at = now(), updated_at = now()
		WHERE site = 'default' AND network_id = 10000 AND service_id = 5000
		  AND event_id = $1 AND deleted_at IS NULL
	`, int32(700004%100000)); err != nil {
		t.Fatalf("soft-deleting real record: %v", err)
	}

	full, err = q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull (after trash): %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— ごみ箱操作で orphaned 表示に戻った（live 限定を掛けてしまっている。issue #318 の「any 行」導出）")
	}

	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation (after trash): %v", err)
	}
	if len(rows) != 0 {
		t.Error("ごみ箱操作後の desired = 1, want 0 —— 同期除外が recordings の状態に依存してしまっている")
	}
}
