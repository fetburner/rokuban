package reconciler_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// never-scheduled の除外条件が予約の再実体化を跨いで効くこと（issue #98）。
//
// 旧 reservations.id は ruler の導出削除・再実体化で変わりうる不安定な値
// （#53 が mirakc の tag を program:{programId} に移した理由。#99 も同じ話）で、
// recordings.reservation_id は当時 ON DELETE SET NULL だった（issue #158 で
// 列自体を削除済み）。除外条件を予約 id で引くと、EPG フリッカーやルール編集で
// 予約行が作り直された瞬間に「never-scheduled 行が無い」ことになり、終了済み
// 予約が毎パス desired に戻り続ける（容量需要にも数え続ける）。宛先のキーは
// 放送イベントと番組開始時刻 (site, network_id, service_id, event_id, program_start_at) でなければならない
// （不変条件 9 の identity: 導出器が作るキーを宛先にしない）。
func TestReconciler_NeverScheduledExclusionSurvivesRematerialization(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := mirakctest.New(t)

	// 終了済みの番組（30 分尺なので 2 時間前開始なら終了済み）。
	startAt := time.Now().Add(-2 * time.Hour)
	res := createReservation(t, pool, 700001, "終了済み番組", startAt)

	r := newReconciler(mirakc.NewClient(mock.URL(), nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass (1st): %v", err)
	}

	// 前提: 欠測行ができ、次パスから desired に現れない。
	rows, err := pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（欠測行で除外されるはず）", len(rows))
	}

	// ruler の導出削除 → 再実体化を模す（同じ番組・同じ複合キー）。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, res.Site, res.ProgramID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	dbtest.ManualReservation(t, pool, 700001)
	res2 := testReservation{Site: "default", ProgramID: 700001}
	rows, err = pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation (after rematerialization): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("再実体化後の desired = %d, want 0 —— 欠測の除外が予約行のキーに依存している（放送イベントで引くべき）", len(rows))
	}

	full, err := pgstoretest.Reservations(pool).GetReservation(ctx, res2.Site, res2.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation: %v", err)
	}
	if !full.NeverRecorded {
		t.Errorf("再実体化後の never_recorded = false, want true —— 表示も予約行のキーに依存している")
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

	// まだ放送中（10 分前開始・30 分尺）。
	res := createReservation(t, pool, 700002, "放送中の番組", time.Now().Add(-10*time.Minute))

	// handleRecordingFailed が作る形の failed 行（never-scheduled マーカーは無い）。
	failedID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.NetworkID = 10000
		p.ServiceID = 5000
		p.EventID = int32(700002 % 100000)
		p.ServiceName = "テスト局"
		p.Title = "放送中の番組"
		p.Status = "failed"
	})
	if _, err := pool.Exec(ctx, `UPDATE recordings SET quality_events = $2::jsonb WHERE id = $1`, failedID,
		`[{"event":"recording.failed","reason":"need-rescheduling"}]`); err != nil {
		t.Fatalf("setting mid-recording failure quality events: %v", err)
	}

	full, err := pgstoretest.Reservations(pool).GetReservation(ctx, res.Site, res.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation: %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— 放送中の失敗で「録れなかった」と表示している（mirakc の再試行待ち）")
	}

	rows, err := pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Reservation.ProgramID == res.ProgramID {
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

	mock := mirakctest.New(t)

	res := createReservation(t, pool, 700003, "後から録れた番組", time.Now().Add(-2*time.Hour))

	r := newReconciler(mirakc.NewClient(mock.URL(), nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	full, err := pgstoretest.Reservations(pool).GetReservation(ctx, res.Site, res.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation: %v", err)
	}
	if !full.NeverRecorded {
		t.Fatalf("precondition: never_recorded = false, want true（欠測行ができているはず）")
	}

	// 前提: 欠測行があるので同期除外も効いている。
	rows, err := pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition: desired = %d, want 0（欠測行で除外されるはず）", len(rows))
	}

	// 本物の record が同じ放送イベントに着地する（watcher の CreateRecording 相当）。
	endedStartAt := time.Now().Add(-2 * time.Hour)
	endedAt := endedStartAt.Add(30 * time.Minute)
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.NetworkID = 10000
		p.ServiceID = 5000
		p.EventID = int32(700003 % 100000)
		p.ServiceName = "テスト局"
		p.Title = "後から録れた番組"
		p.ProgramStartAt = endedStartAt
		p.StartedAt = &endedStartAt
		p.EndedAt = &endedAt
	})

	full, err = pgstoretest.Reservations(pool).GetReservation(ctx, res.Site, res.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation (after real record): %v", err)
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
	rows, err = pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation (after real record): %v", err)
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

	mock := mirakctest.New(t)

	res := createReservation(t, pool, 700004, "録れた後ごみ箱に入れた番組", time.Now().Add(-2*time.Hour))

	r := newReconciler(mirakc.NewClient(mock.URL(), nil), pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	// 本物の record が着地。
	trashedStartAt := time.Now().Add(-2 * time.Hour)
	trashedEndedAt := trashedStartAt.Add(30 * time.Minute)
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.NetworkID = 10000
		p.ServiceID = 5000
		p.EventID = int32(700004 % 100000)
		p.ServiceName = "テスト局"
		p.Title = "録れた後ごみ箱に入れた番組"
		p.ProgramStartAt = trashedStartAt
		p.StartedAt = &trashedStartAt
		p.EndedAt = &trashedEndedAt
	})

	full, err := pgstoretest.Reservations(pool).GetReservation(ctx, res.Site, res.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation: %v", err)
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

	full, err = pgstoretest.Reservations(pool).GetReservation(ctx, res.Site, res.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation (after trash): %v", err)
	}
	if full.NeverRecorded {
		t.Error("never_recorded = true, want false —— ごみ箱操作で orphaned 表示に戻った（live 限定を掛けてしまっている。issue #318 の「any 行」導出）")
	}

	rows, err := pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation (after trash): %v", err)
	}
	if len(rows) != 0 {
		t.Error("ごみ箱操作後の desired = 1, want 0 —— 同期除外が recordings の状態に依存してしまっている")
	}
}

func insertNeverScheduledAtStart(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID int32, startAt time.Time) {
	t.Helper()
	created, err := pgstoretest.NeverScheduledEvents(pool).RecordNeverScheduled(ctx, "default", store.BroadcastEventKey{
		NetworkID: 10000, ServiceID: 5000, EventID: eventID,
	}, startAt)
	if err != nil {
		t.Fatalf("RecordNeverScheduled: %v, want nil", err)
	}
	if !created {
		t.Fatalf("RecordNeverScheduled = %t, want true", created)
	}
}

func hasNeverScheduledAtStart(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID int32, startAt time.Time) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM never_scheduled_events
    WHERE site = 'default' AND network_id = 10000 AND service_id = 5000 AND event_id = $1
      AND program_start_at = $2
)`, eventID, startAt).Scan(&exists); err != nil {
		t.Fatalf("checking never-scheduled event at program start: %v", err)
	}
	return exists
}

func assertProgramIDs(t *testing.T, name string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s program IDs = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s program IDs = %v, want %v", name, got, want)
		}
	}
}

// 同じ放送イベントでも番組開始時刻が違えば別の欠測 identity として扱う。
// A の欠測行は A 自身を除外し、B には表示・同期・容量・重なり・プレビューの
// いずれでも影響しない。7 つの読者すべてを実 DB に対して通す。
func TestNeverScheduledReadersScopeByProgramStartAt(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	startA := time.Now().UTC().Truncate(time.Microsecond).Add(-3 * time.Hour)
	startB := startA.Add(10 * time.Minute)
	const eventID int32 = 4321
	const programIDA int64 = 704321
	const programIDB int64 = 804321
	const programIDCandidate int64 = 904321

	resA := createReservation(t, pool, programIDA, "旧番組 A", startA)
	resB := createReservation(t, pool, programIDB, "新番組 B", startB)
	insertNeverScheduledAtStart(t, ctx, pool, eventID, startA)

	syncRows, err := pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation: %v", err)
	}
	var syncIDs []int64
	for _, row := range syncRows {
		syncIDs = append(syncIDs, row.Reservation.ProgramID)
	}
	assertProgramIDs(t, "ListForSyncEvaluation", syncIDs, []int64{resB.ProgramID})

	fullRows, err := pgstoretest.Reservations(pool).ListReservations(ctx)
	if err != nil {
		t.Fatalf("ListReservations: %v", err)
	}
	if len(fullRows) != 2 {
		t.Fatalf("ListReservations rows = %d, want 2", len(fullRows))
	}
	fullNeverRecorded := make(map[int64]bool, len(fullRows))
	for _, row := range fullRows {
		fullNeverRecorded[row.Reservation.ProgramID] = row.NeverRecorded
	}
	if !fullNeverRecorded[resA.ProgramID] || fullNeverRecorded[resB.ProgramID] {
		t.Fatalf("ListReservations never_recorded = %v, want A=true and B=false", fullNeverRecorded)
	}

	for _, test := range []struct {
		name string
		id   int64
		want bool
	}{{"A", resA.ProgramID, true}, {"B", resB.ProgramID, false}} {
		row, err := pgstoretest.Reservations(pool).GetReservation(ctx, "default", test.id)
		if err != nil {
			t.Fatalf("GetReservation(%s): %v", test.name, err)
		}
		if row.NeverRecorded != test.want {
			t.Errorf("GetReservation(%s).never_recorded = %t, want %t", test.name, row.NeverRecorded, test.want)
		}
	}

	demandInput, err := pgstoretest.Tuners(pool).ReadSite(ctx, "default")
	if err != nil {
		t.Fatalf("ReadSite: %v", err)
	}
	if len(demandInput.Demands) != 1 || !demandInput.Demands[0].StartAt.Equal(startB) {
		t.Fatalf("ReadSite demands = %+v, want only B at %s", demandInput.Demands, startB)
	}

	demandAllInput, err := pgstoretest.Tuners(pool).ReadAllSites(ctx)
	if err != nil {
		t.Fatalf("ReadAllSites: %v", err)
	}
	if len(demandAllInput.Demands) != 1 || !demandAllInput.Demands[0].StartAt.Equal(startB) {
		t.Fatalf("ReadAllSites demands = %+v, want only B at %s", demandAllInput.Demands, startB)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel)
VALUES ('default', 10000, 5000, 1, 0, 1, 'テスト局', 'GR', '27')`); err != nil {
		t.Fatalf("inserting EPG service for preview: %v", err)
	}
	startCandidate := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at)
VALUES ('default', $1, 10000, 5000, $2, $3::timestamptz, 1800000, $3::timestamptz + interval '30 minutes')`,
		programIDCandidate, eventID, startCandidate); err != nil {
		t.Fatalf("inserting EPG preview candidate: %v", err)
	}
	previewRows, err := pgstoretest.Tuners(pool).PreviewCandidates(ctx, []store.CapacityProgramRef{{Site: "default", ProgramID: programIDCandidate}}, nil)
	if err != nil {
		t.Fatalf("PreviewCandidates: %v", err)
	}
	if len(previewRows) != 1 || previewRows[0].ProgramID != programIDCandidate {
		t.Fatalf("PreviewCandidates rows = %+v, want candidate %d", previewRows, programIDCandidate)
	}

	const overlapTargetID int64 = 999999
	const overlapTargetEventID int32 = 9876
	targetStartAt := startA.Add(-time.Minute)
	targetEndAt := startB.Add(time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at)
VALUES ('default', $1, 10000, 5000, $2, $3::timestamptz, $4, $5::timestamptz)`,
		overlapTargetID, overlapTargetEventID, targetStartAt, targetEndAt.Sub(targetStartAt).Milliseconds(), targetEndAt); err != nil {
		t.Fatalf("inserting EPG overlap target: %v", err)
	}
	overlapRows, err := pgstoretest.Reservations(pool).ListOverlaps(ctx, "default", overlapTargetID)
	if err != nil {
		t.Fatalf("ListOverlaps: %v", err)
	}
	var overlapIDs []int64
	for _, row := range overlapRows {
		overlapIDs = append(overlapIDs, row.ProgramID)
	}
	assertProgramIDs(t, "ListOverlaps", overlapIDs, []int64{resB.ProgramID})
}

// snapshot の start_at を渡す書き手が、同じ event_id の B について別の欠測行を
// 作り、実際に新規行を作ったときだけ B のログを出すことを確認する。
func TestReconciler_RecordsReusedEventIDAtSnapshotStartAndLogs(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	startA := time.Now().UTC().Truncate(time.Microsecond).Add(-3 * time.Hour)
	startB := startA.Add(10 * time.Minute)
	const eventID int32 = 5432
	const programIDA int64 = 705432
	const programIDB int64 = 805432

	createReservation(t, pool, programIDA, "旧番組 A", startA)
	resB := createReservation(t, pool, programIDB, "新番組 B", startB)
	insertNeverScheduledAtStart(t, ctx, pool, eventID, startA)
	fullB, err := pgstoretest.Reservations(pool).GetReservation(ctx, "default", resB.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation(B before reconcile): %v", err)
	}
	if fullB.NeverRecorded {
		t.Fatal("B.never_recorded = true before B's own observation, want false")
	}

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	mock := mirakctest.New(t)
	if err := newReconciler(mirakc.NewClient(mock.URL(), nil), pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if !hasNeverScheduledAtStart(t, ctx, pool, eventID, startA) ||
		!hasNeverScheduledAtStart(t, ctx, pool, eventID, startB) {
		t.Fatal("expected separate never-scheduled rows for A and B at their snapshot start times")
	}
	if !bytes.Contains(logs.Bytes(), []byte("program_id=805432")) ||
		!bytes.Contains(logs.Bytes(), []byte("recorded never-scheduled outcome")) {
		t.Errorf("expected B's new never-scheduled observation in the log, got:\n%s", logs.String())
	}
	if calls := scheduleCallIDs(mock, http.MethodPost); len(calls) != 0 {
		t.Errorf("POST schedule calls = %v, want none for ended B", calls)
	}
}

// snapshot start_at は終了後も更新されうる。古い開始時刻の欠測は新しい snapshot に
// 一度だけ一致しなくなるが、終了済みガードが schedule POST を止め、同じパスで
// 新しい開始時刻の欠測を記録する。次の同期評価では予約が再び除外される。
func TestReconciler_NeverScheduledSnapshotStartChangeIsReobservedWithoutSchedulePost(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	startA := time.Now().UTC().Truncate(time.Microsecond).Add(-4 * time.Hour)
	startB := startA.Add(time.Hour)
	const programID int64 = 706543
	const eventID int32 = 6543

	res := createReservation(t, pool, programID, "開始時刻変更後も終了済みの番組", startA)
	insertNeverScheduledAtStart(t, ctx, pool, eventID, startA)
	rows, err := pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation before snapshot update: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("precondition candidates = %d, want 0", len(rows))
	}
	dbtest.ProgramSnapshot(t, pool, res.ProgramID, func(p *dbtest.ProgramSnapshotParams) {
		p.Title = "開始時刻変更後も終了済みの番組"
		p.StartAt = startB
		p.NetworkID = 10000
		p.ServiceID = 5000
		p.EventID = eventID
	})
	rows, err = pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation after snapshot update: %v", err)
	}
	if len(rows) != 1 || rows[0].Reservation.ProgramID != res.ProgramID {
		t.Fatalf("candidate after snapshot update = %+v, want the ended reservation to be reconsidered", rows)
	}

	mock := mirakctest.New(t)
	if err := newReconciler(mirakc.NewClient(mock.URL(), nil), pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass after snapshot update: %v", err)
	}
	if calls := scheduleCallIDs(mock, http.MethodPost); len(calls) != 0 {
		t.Fatalf("POST schedule calls = %v, want none for ended reservation", calls)
	}
	if !hasNeverScheduledAtStart(t, ctx, pool, eventID, startA) ||
		!hasNeverScheduledAtStart(t, ctx, pool, eventID, startB) {
		t.Fatal("expected the old and updated snapshot start times to retain separate observations")
	}
	rows, err = pgstoretest.ScheduleSync(pool).ListForSyncEvaluation(ctx, "default")
	if err != nil {
		t.Fatalf("ListForSyncEvaluation after reobservation: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("candidates after reobservation = %d, want 0", len(rows))
	}
}
