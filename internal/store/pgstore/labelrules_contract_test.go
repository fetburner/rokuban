package pgstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var labelRuleContractEventID atomic.Int32

func TestLabelRulesContract(t *testing.T) {
	contracttest.RunLabelRules(t, func(t *testing.T) contracttest.LabelRuleFixture {
		pool := testutil.SetupDB(t)
		labelRules := NewLabelRules(pool)
		return contracttest.LabelRuleFixture{
			Store:        labelRules,
			Reevaluation: labelRules,
			NewRecording: func(t *testing.T, title string) int64 {
				t.Helper()
				id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
					Source: "manual", Site: db.DefaultSite, NetworkID: 32678, ServiceID: 5168,
					EventID: labelRuleContractEventID.Add(1), ServiceName: "ＯＨＫ", ChannelType: "GR", Channel: "27",
					Title: title, ProgramStartAt: time.Now().UTC().Truncate(time.Second),
					ProgramDurationMs: 30 * time.Minute.Milliseconds(), Status: "finished",
				})
				if err != nil {
					t.Fatalf("seeding recording: %v", err)
				}
				return id
			},
			BeginReevaluationLock: func(t *testing.T) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning re-evaluation lock tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback(ctx) })
				if err := sqlcgen.New(tx).LockLabelRuleReevaluation(ctx); err != nil {
					t.Fatalf("acquiring re-evaluation lock: %v", err)
				}
				return func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("committing re-evaluation lock tx: %v", err)
					}
				}
			},
			WaitForReevaluationLock: func(t *testing.T) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var waiting bool
					if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM pg_locks
    WHERE locktype = 'advisory' AND NOT granted
)`).Scan(&waiting); err != nil {
						t.Fatalf("checking for a waiting re-evaluation: %v", err)
					}
					if waiting {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("ReevaluateAll did not wait for the held re-evaluation lock")
			},
			LabelRuleHit: func(t *testing.T, recordingID int64) (ruleID int64, rowVersion string, exists bool) {
				t.Helper()
				err := pool.QueryRow(context.Background(), `
SELECT label_rule_id, ctid::text
FROM label_rule_hits
WHERE recording_id = $1`, recordingID).Scan(&ruleID, &rowVersion)
				if errors.Is(err, pgx.ErrNoRows) {
					return 0, "", false
				}
				if err != nil {
					t.Fatalf("reading label rule hit for recording %d: %v", recordingID, err)
				}
				return ruleID, rowVersion, true
			},
		}
	})
}

func TestReevaluateLabelRulesTxNotifiesOnlyOnChange(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingIDs := []int64{
		createLabelRuleTestRecording(t, pool, 9876, "作品X 第1話"),
		createLabelRuleTestRecording(t, pool, 9877, "作品X 第2話"),
	}
	if _, err := sqlcgen.New(pool).CreateLabelRule(ctx, sqlcgen.CreateLabelRuleParams{
		Key: "series", Value: "作品X", Keyword: "作品X",
	}); err != nil {
		t.Fatalf("creating label rule: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION public.test_pause_label_rule_hit() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_sleep(1);
    RETURN NEW;
END
$$`); err != nil {
		t.Fatalf("creating label rule hit pause trigger function: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS label_rule_hits_test_pause ON label_rule_hits`); err != nil {
			t.Errorf("dropping label rule hit pause trigger: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS public.test_pause_label_rule_hit()`); err != nil {
			t.Errorf("dropping label rule hit pause trigger function: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
CREATE TRIGGER label_rule_hits_test_pause
AFTER INSERT ON label_rule_hits
FOR EACH ROW EXECUTE FUNCTION public.test_pause_label_rule_hit()`); err != nil {
		t.Fatalf("creating label rule hit pause trigger: %v", err)
	}

	listenConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(ctx, "LISTEN rokuban"); err != nil {
		t.Fatal(err)
	}
	store := NewLabelRules(pool)
	done := make(chan struct {
		changed int64
		err     error
	}, 1)
	go func() {
		changed, err := store.ReevaluateAll(ctx)
		done <- struct {
			changed int64
			err     error
		}{changed: changed, err: err}
	}()
	pid := waitForLabelRuleHitInsert(t, pool)
	var holdsReevaluationLock bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_locks
    WHERE pid = $1 AND locktype = 'advisory' AND granted
)`, pid).Scan(&holdsReevaluationLock); err != nil {
		t.Fatalf("checking the re-evaluation lock during hit writes: %v", err)
	}
	if !holdsReevaluationLock {
		t.Fatal("the hit write did not hold the re-evaluation lock in its transaction")
	}
	assertNoCommittedLabelRuleHits(t, pool, ctx, recordingIDs)

	var result struct {
		changed int64
		err     error
	}
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	select {
	case result = <-done:
	case <-timeout.C:
		t.Fatal("ReevaluateAll did not finish after the hit writes")
	}

	if result.err != nil || result.changed != 2 {
		t.Fatalf("ReevaluateAll = %d, %v, want 2, nil", result.changed, result.err)
	}
	if got := labelRuleHitCount(t, pool, ctx, recordingIDs); got != 2 {
		t.Fatalf("committed label rule hit count = %d, want 2", got)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	notification, err := listenConn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("waiting for changed re-evaluation notification: %v", err)
	}
	if notification.Payload != "recordings" {
		t.Errorf("notification = %q, want recordings", notification.Payload)
	}

	if changed, err := store.ReevaluateAll(ctx); err != nil || changed != 0 {
		t.Fatalf("ReevaluateAll unchanged = %d, %v, want 0, nil", changed, err)
	}
	assertNoNotification(t, listenConn)
}

func createLabelRuleTestRecording(t *testing.T, pool *pgxpool.Pool, eventID int32, title string) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source: "manual", Site: db.DefaultSite, NetworkID: 32678, ServiceID: 5168, EventID: eventID,
		ServiceName: "ＯＨＫ", ChannelType: "GR", Channel: "27", Title: title,
		ProgramStartAt: time.Now().UTC().Truncate(time.Second), ProgramDurationMs: 60000, Status: "finished",
	})
	if err != nil {
		t.Fatalf("creating recording %d: %v", eventID, err)
	}
	return id
}

func waitForLabelRuleHitInsert(t *testing.T, pool *pgxpool.Pool) int32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pid int32
		err := pool.QueryRow(context.Background(), `
SELECT pid
FROM pg_stat_activity
WHERE state = 'active'
  AND wait_event = 'PgSleep'
  AND query LIKE '%INSERT INTO label_rule_hits%'
  AND pid <> pg_backend_pid()
LIMIT 1`).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("finding the re-evaluation during its hit write: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("label rule hit insertion did not enter its pause trigger")
	return 0
}

func assertNoCommittedLabelRuleHits(t *testing.T, pool *pgxpool.Pool, ctx context.Context, recordingIDs []int64) {
	t.Helper()
	if got := labelRuleHitCount(t, pool, ctx, recordingIDs); got != 0 {
		t.Fatalf("visible label rule hits during re-evaluation = %d, want 0", got)
	}
}

func labelRuleHitCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, recordingIDs []int64) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM label_rule_hits WHERE recording_id = ANY($1)`, recordingIDs).Scan(&count); err != nil {
		t.Fatalf("counting label rule hits: %v", err)
	}
	return count
}

func assertNoNotification(t *testing.T, conn *pgxpool.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if notification, err := conn.Conn().WaitForNotification(ctx); err == nil {
		t.Fatalf("unexpected notification %q", notification.Payload)
	}
}
