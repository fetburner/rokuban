package ruler_test

import (
	"context"
	"testing"
	"time"

	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// ruler_pass ジョブが投入され、実際に実行されて予約が作られること
// （ロジック自体は internal/ruler でテスト済みなので、ここでは「ジョブとして
// 呼ばれると本当に動く」という配線だけを確認する）。
func TestPassWorker_CreatesReservation(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	const site = "default"
	const networkID, serviceID int32 = 32736, 1024
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, $2, $3, 1, 0, 1, 'テスト局', 'GR', '27', false)`, site, networkID, serviceID); err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}

	const programID int64 = 1
	startAt := time.Now().Add(time.Hour)
	const durationMs int64 = 1800000
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, is_free, name, description)
VALUES ($1, $2, $3, $4, 0, $5, $6, $7, true, 'テスト番組', '')`,
		site, programID, networkID, serviceID, startAt, durationMs, startAt.Add(time.Duration(durationMs)*time.Millisecond)); err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}

	var ruleID int64
	if err := pool.QueryRow(ctx, `INSERT INTO rules (name, priority) VALUES ('全部録る', 10) RETURNING id`).Scan(&ruleID); err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, 0, 'name', 'keyword', 'テスト', true, false)`, ruleID); err != nil {
		t.Fatalf("inserting rule_text_matches fixture: %v", err)
	}

	reservations := pgstoretest.Reservations(pool)
	w := &ruler.PassWorker{
		Stores:         ruler.Stores{RulerPass: reservations, ProgramSearch: pgstoretest.EPG(pool)},
		Breakers:       pgstoretest.Breakers(pool),
		NeverScheduled: pgstoretest.NeverScheduledEvents(pool),
		LoopMarkers:    reservations,
	}
	if err := jobqtestutil.RunWorkerOnce(ctx, w, jobs.RulerPassArgs{Site: site}); err != nil {
		t.Fatalf("PassWorker.Work: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2`, site, programID,
	).Scan(&count); err != nil {
		t.Fatalf("querying reservations: %v", err)
	}
	if count != 1 {
		t.Fatalf("reservations count = %d, want 1 (ruler_pass ジョブがルール評価を実行して予約を作ったはず)", count)
	}

	var markerCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM loop_success_markers WHERE loop = 'ruler' AND scope = $1`, site,
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying ruler pass marker: %v", err)
	}
	if markerCount != 1 {
		t.Errorf("ruler pass marker rows = %d, want 1", markerCount)
	}
}

// ruler の処理が失敗したときは、ScaledJob の再試行を促すため marker を進めない。
func TestPassWorker_DoesNotMarkFailedPass(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reservations := pgstoretest.Reservations(pool)
	w := &ruler.PassWorker{
		Stores:         ruler.Stores{RulerPass: reservations, ProgramSearch: pgstoretest.EPG(pool)},
		Breakers:       pgstoretest.Breakers(pool),
		NeverScheduled: pgstoretest.NeverScheduledEvents(pool),
		LoopMarkers:    reservations,
	}
	job := jobs.RulerPassArgs{Site: "default"}
	if err := jobqtestutil.RunWorkerOnce(ctx, w, job); err == nil {
		t.Fatal("Work() error = nil, want canceled ruler pass to fail")
	}

	var markerCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM loop_success_markers WHERE loop = 'ruler' AND scope = $1`, "default",
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying ruler pass marker: %v", err)
	}
	if markerCount != 0 {
		t.Errorf("ruler pass marker rows after failed pass = %d, want 0", markerCount)
	}
}

// ruler パスの完了は reconcile_pass 起動契機のヒントの 1 つ（docs/recording.md
// §3.2「ruler パスの完了」）。base が変われば mirakc に反映すべき差分が増えるため、
// reconcile を早める。
func TestPassWorker_EnqueuesReconcilePassHint(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	reservations := pgstoretest.Reservations(pool)
	w := &ruler.PassWorker{
		Stores:         ruler.Stores{RulerPass: reservations, ProgramSearch: pgstoretest.EPG(pool)},
		Breakers:       pgstoretest.Breakers(pool),
		NeverScheduled: pgstoretest.NeverScheduledEvents(pool),
		LoopMarkers:    reservations,
		Enqueuer:       enqueuer,
	}
	if err := jobqtestutil.RunWorkerOnce(ctx, w, jobs.RulerPassArgs{Site: testSite}); err != nil {
		t.Fatalf("PassWorker.Work: %v", err)
	}

	var hints []jobs.ReconcilePassArgs
	for _, call := range enqueuer.CallsOfKind(jobs.ReconcilePassArgs{}.Kind()) {
		if args, ok := call.Args.(jobs.ReconcilePassArgs); ok {
			hints = append(hints, args)
		}
	}
	if len(hints) != 1 || hints[0].Site != testSite {
		t.Fatalf("enqueued reconcile_pass hints = %+v, want one hint for site %q", hints, testSite)
	}
}
