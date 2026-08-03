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
// recordings.reservation_id は ON DELETE SET NULL である。除外条件を
// reservation_id で引くと、EPG フリッカーやルール編集で予約行が作り直された
// 瞬間に「never-scheduled 行が無い」ことになり、終了済み予約が毎パス desired に
// 戻り続ける（容量需要にも数え続ける）。宛先のキーは放送イベント
// (site, network_id, service_id, event_id) でなければならない（不変条件 9 の
// identity: 導出器が作るキーを宛先にしない）。
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
			reservation_id, source, site, network_id, service_id, event_id,
			service_name, channel_type, channel, title, is_free,
			program_start_at, program_duration_ms, status, quality_events
		) VALUES ($1, 'manual', 'default', 10000, 5000, $2,
			'テスト局', 'GR', '27', '放送中の番組', true,
			now(), 1800000, 'failed',
			'[{"event":"recording.failed","reason":"need-rescheduling"}]'::jsonb)
	`, res.ID, int32(700002%100000)); err != nil {
		t.Fatalf("inserting mid-recording failure: %v", err)
	}

	full, err := q.GetReservationFull(ctx, res.ID)
	if err != nil {
		t.Fatalf("GetReservationFull: %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— 放送中の失敗で「録れなかった」と表示している（mirakc の再試行待ち）")
	}
}

// never-scheduled 行が supersede されたら「録れなかった」表示が消えること（#59 / #98）。
//
// 後から本物の record が着地すると never-scheduled 行は supersede される
// （#129 / #143 の「本物の record が推論に必ず勝つ」）。表示の述語が live な行に
// 限っているので、「録れたのに orphaned のまま」（#59）が構造的に消える。
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
}
