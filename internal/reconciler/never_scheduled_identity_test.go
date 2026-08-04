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

	// 前提: never-scheduled 行ができ、次パスから desired に現れない。
	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（never-scheduled 行で除外されるはず）", len(rows))
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
		t.Errorf("再実体化後の desired = %d, want 0 —— never-scheduled の除外が予約 id に依存している（放送イベントで引くべき）", len(rows))
	}

	full, err := q.GetReservationFull(ctx, res2.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if !full.NeverRecorded {
		t.Errorf("再実体化後の never_recorded = false, want true —— 表示も予約 id に依存している")
	}
}

// 放送中の mirakc 由来の失敗では「録れなかった」表示にしないこと（issue #98）。
// 同期除外（ListReservationsForSyncEvaluation）も同じマーカー限定を共有する
// ので、この失敗で desired から除外されないことも合わせて確認する（issue #157:
// never_scheduled_events view の述語が status='failed' 全般に緩んでいないか
// の回帰確認。表示側と同期除外側の両方が同じ view を参照するので、この 1 つの
// fixture で両方の消費者をカバーできる）。
//
// 旧 orphaned_at は「番組終了かつ schedule 非観測」でしか立たなかった。
// never_recorded を「status='failed' の行があるか」で導出すると、放送中の番組が
// mirakc の再スケジュール待ちの間に orphaned と表示されて退行する。
// never-scheduled マーカーに限ることで避けている。
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
		t.Error("放送中の mirakc 由来の失敗（never-scheduled マーカー無し）で desired から除外された --- " +
			"再試行経路が壊れている（never_scheduled_events view が status='failed' 全般まで拾っている疑い）")
	}
}

// never-scheduled 行が supersede されたら「録れなかった」表示が消えること（#59 / #98）。
//
// 後から本物の record が着地すると never-scheduled 行は supersede される
// （#129 / #143 の「本物の record が推論に必ず勝つ」）。表示の述語が live な行に
// 限っているので、「録れたのに orphaned のまま」（#59）が構造的に消える。
//
// 一方で同期除外（ListReservationsForSyncEvaluation）は live 限定
// （deleted_at / superseded_at）を持たない --- issue #157 が
// never_scheduled_events VIEW（00030）に畳み込んではならないと明記した差。
// supersede で表示（never_recorded）は変わっても、同期除外の対象（desired）は
// 変わらないことをここで確認する。VIEW の WHERE に
// `AND deleted_at IS NULL AND superseded_at IS NULL` を足すと、この予約が
// 毎パス desired に戻り続ける経路が開き、このアサーションが落ちる。
func TestReservation_SupersededNeverScheduledClearsOrphanedDisplay(t *testing.T) {
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
		t.Fatalf("precondition: never_recorded = false, want true（never-scheduled 行ができているはず）")
	}

	// 前提: never-scheduled 行がまだ live なので、同期除外も効いている。
	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（never-scheduled 行で除外されるはず）", len(rows))
	}

	// 本物の record が着地して never-scheduled 行を supersede する。
	n, err := q.SupersedeFailedRecording(ctx, sqlcgen.SupersedeFailedRecordingParams{
		Site: "default", NetworkID: 10000, ServiceID: 5000, EventID: int32(700003 % 100000),
	})
	if err != nil || n != 1 {
		t.Fatalf("SupersedeFailedRecording: rows=%d err=%v", n, err)
	}

	full, err = q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull (after supersede): %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— supersede された never-scheduled 行で「録れなかった」と表示し続けている（#59 の再発）")
	}

	// 表示（never_recorded）は supersede で変わるが、同期除外は live 限定を
	// 持たないので変わらないはず（issue #157 の意図的な差）。
	rows, err = q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation (after supersede): %v", err)
	}
	if len(rows) != 0 {
		t.Error("supersede 後の desired = 1, want 0 —— 同期除外が live 限定（superseded_at）を持ってしまっている（issue #157 が禁じた畳み込み）")
	}
}

// never-scheduled 行を論理削除（ごみ箱）しても同期除外が外れないこと（issue #157）。
//
// 表示用 never_recorded は live 限定（deleted_at IS NULL）を持つので、ごみ箱に
// 入れた never-scheduled 行は表示上「録れた」扱いに戻ってよい。しかし同期除外
// （ListReservationsForSyncEvaluation）は live 限定を持たない --- ユーザーが
// ごみ箱に入れた・入れていないという操作で、reconciler が同じ番組の予約を
// desired に戻して再スケジュールを試みてはならない。VIEW の WHERE に
// `AND deleted_at IS NULL` を足すと、このアサーションが落ちる。
func TestReservation_DeletedNeverScheduledDoesNotReenterSyncCandidates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	q := sqlcgen.New(pool)
	res := createReservation(t, ctx, q, 700004, "ごみ箱に入れた番組", time.Now().Add(-2*time.Hour))

	r := reconciler.New("default", mirakc.NewClient(srv.URL, nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	full, err := q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if !full.NeverRecorded {
		t.Fatalf("precondition: never_recorded = false, want true（never-scheduled 行ができているはず）")
	}

	rows, err := q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（never-scheduled 行で除外されるはず）", len(rows))
	}

	// never-scheduled 行を論理削除する（ごみ箱に入れる。recordings_trash.sql の
	// SoftDeleteRecording と同じ更新。id を直接引く手段が無いので放送イベントで
	// 更新する --- テスト用の直接更新であり、宛先のキーの話ではない）。
	if _, err := pool.Exec(ctx, `
		UPDATE recordings SET deleted_at = now(), updated_at = now()
		WHERE site = 'default' AND network_id = 10000 AND service_id = 5000
		  AND event_id = $1 AND status = 'failed' AND deleted_at IS NULL
	`, int32(700004%100000)); err != nil {
		t.Fatalf("soft-deleting never-scheduled recording: %v", err)
	}

	full, err = q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull (after soft-delete): %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— ごみ箱に入れた never-scheduled 行で「録れなかった」と表示し続けている（表示側の live 限定が効いていない）")
	}

	rows, err = q.ListReservationsForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListReservationsForSyncEvaluation (after soft-delete): %v", err)
	}
	if len(rows) != 0 {
		t.Error("ごみ箱に入れた後の desired = 1, want 0 —— 同期除外が live 限定（deleted_at）を持ってしまっている（issue #157 が禁じた畳み込み）")
	}
}
