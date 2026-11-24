package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func countNotifications(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatalf("counting notifications: %v", err)
	}
	return n
}

func TestNotifyTopicWritesRow(t *testing.T) {
	db := mysqltest.SetupDB(t)
	if err := NotifyTopic(context.Background(), db, "recordings"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	var payload string
	if err := db.QueryRowContext(context.Background(), `SELECT payload FROM notifications`).Scan(&payload); err != nil {
		t.Fatalf("reading notification: %v", err)
	}
	if payload != "recordings" {
		t.Errorf("payload = %q, want recordings", payload)
	}
}

// TestNotifyTopicRollsBackWithTransaction は、通知がデータと同じ tx に入ることを検査する。
// tx の commit を外して auto-commit で書く変異は、rollback 後の行数で落ちる。
func TestNotifyTopicRollsBackWithTransaction(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	err := runTx(ctx, db, func(tx *sql.Tx) error {
		if err := NotifyTopic(ctx, tx, "recordings"); err != nil {
			return err
		}
		return context.Canceled // 強制 rollback
	})
	if err == nil {
		t.Fatal("runTx returned nil, want the forced error")
	}
	if n := countNotifications(t, db); n != 0 {
		t.Errorf("rows after rollback = %d, want 0", n)
	}
}

func TestNotifyTopicRejectsOversizedPayload(t *testing.T) {
	db := mysqltest.SetupDB(t)
	err := NotifyTopic(context.Background(), db, strings.Repeat("a", maxNotificationBytes+1))
	if err == nil {
		t.Fatal("NotifyTopic accepted a payload over the byte limit")
	}
	if n := countNotifications(t, db); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// TestCircuitBreakerTriggersNotify は、circuit_breakers の INSERT・UPDATE・DELETE それぞれが
// "breakers" を 1 行ずつ書くことを検査する。3 本のうち 1 本を外すと、対応する操作の行数で落ちる。
func TestCircuitBreakerTriggersNotify(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	breakers := NewBreakers(db)
	sample := store.BreakerSample{Total: 1}

	if _, err := breakers.Trip(ctx, "site", "a", 3, sample); err != nil {
		t.Fatalf("Trip insert: %v", err)
	}
	if n := countNotifications(t, db); n != 1 {
		t.Fatalf("rows after INSERT = %d, want 1", n)
	}
	if _, err := breakers.Trip(ctx, "site", "a", 3, store.BreakerSample{Total: 2}); err != nil {
		t.Fatalf("Trip update: %v", err)
	}
	if n := countNotifications(t, db); n != 2 {
		t.Fatalf("rows after UPDATE = %d, want 2", n)
	}
	if err := breakers.Resume(ctx, "site", "a"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n := countNotifications(t, db); n != 3 {
		t.Fatalf("rows after DELETE = %d, want 3", n)
	}
	var topics int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE payload = 'breakers'`).Scan(&topics); err != nil {
		t.Fatalf("counting breakers topics: %v", err)
	}
	if topics != 3 {
		t.Errorf("breakers topics = %d, want 3", topics)
	}
}

// TestPruneNotificationsRemovesOnlyOldRows は、掃除が保持期間より古い行だけを消すことを検査する。
// created_at を明示して、保持期間より十分古い行と十分新しい行を入れる（時刻の境界には頼らない）。
func TestPruneNotificationsRemovesOnlyOldRows(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	old := NotificationRetention * 2
	for range 3 {
		if _, err := db.ExecContext(ctx, `INSERT INTO notifications (payload, created_at) VALUES ('rules', NOW(6) - INTERVAL ? MICROSECOND)`, old.Microseconds()); err != nil {
			t.Fatalf("inserting old notification: %v", err)
		}
	}
	if err := NotifyTopic(ctx, db, "recordings"); err != nil {
		t.Fatalf("NotifyTopic new: %v", err)
	}
	deleted, err := PruneNotifications(ctx, db)
	if err != nil {
		t.Fatalf("PruneNotifications: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want the 3 old rows", deleted)
	}
	if n := countNotifications(t, db); n != 1 {
		t.Errorf("rows = %d, want only the new row", n)
	}
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT payload FROM notifications`).Scan(&payload); err != nil {
		t.Fatalf("reading remaining row: %v", err)
	}
	if payload != "recordings" {
		t.Errorf("remaining payload = %q, want recordings", payload)
	}
}

// TestReservationTriggersNotify は、各 DML の通知と rollback 時の取り消しを検査する。
func TestReservationTriggersNotify(t *testing.T) {
	cases := []struct {
		table, topic, insert, update, delete string
	}{
		{"reservations", "reservations",
			"INSERT INTO reservations (site, program_id) VALUES (?, ?)",
			"UPDATE reservations SET base = JSON_OBJECT('priority', 2) WHERE site = ? AND program_id = ?",
			"DELETE FROM reservations WHERE site = ? AND program_id = ?"},
		{"program_intents", "reservations",
			"INSERT INTO program_intents (site, program_id, action) VALUES (?, ?, 'record')",
			"UPDATE program_intents SET action = 'skip' WHERE site = ? AND program_id = ?",
			"DELETE FROM program_intents WHERE site = ? AND program_id = ?"},
		{"program_overrides", "reservations",
			"INSERT INTO program_overrides (site, program_id, overrides) VALUES (?, ?, JSON_OBJECT('priority', 1))",
			"UPDATE program_overrides SET overrides = JSON_OBJECT('priority', 2) WHERE site = ? AND program_id = ?",
			"DELETE FROM program_overrides WHERE site = ? AND program_id = ?"},
		{"media_assets", "recordings",
			"INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 1)",
			"UPDATE media_assets SET size_bytes = 2 WHERE recording_id = ? AND kind = 'original'",
			"DELETE FROM media_assets WHERE recording_id = ? AND kind = 'original'"},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			db := mysqltest.SetupDB(t)
			ctx := t.Context()
			p := seedReservationProgram(t, db, "notification fixture", contracttest.ReservationProgramOptions{Snapshot: true})
			args := []any{p.Site, p.ProgramID}
			if c.table == "media_assets" {
				rule := fixtureID(t, execFixture(t, db, "INSERT INTO rules (name) VALUES ('notification fixture')"))
				args = []any{seedFinishedRecording(t, db, rule, p, p.Title)}
				execFixture(t, db, "DELETE FROM notifications")
			}
			forcedRollback := errors.New("forced rollback")
			for i, op := range []struct{ name, query string }{
				{"INSERT", c.insert}, {"UPDATE", c.update}, {"DELETE", c.delete},
			} {
				err := runTx(ctx, db, func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, op.query, args...); err != nil {
						return err
					}
					return forcedRollback
				})
				_, hasJoinedErrors := errors.AsType[interface {
					error
					Unwrap() []error
				}](err)
				if !errors.Is(err, forcedRollback) || hasJoinedErrors {
					t.Fatalf("%s rollback: %v", op.name, err)
				}
				if n := countNotifications(t, db); n != i {
					t.Fatalf("total after rolled-back %s = %d, want %d", op.name, n, i)
				}
				execFixture(t, db, op.query, args...)
				if n := countNotifications(t, db); n != i+1 {
					t.Fatalf("total after committed %s = %d, want %d", op.name, n, i+1)
				}
				var matching int
				if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifications WHERE payload = ?", c.topic).Scan(&matching); err != nil {
					t.Fatalf("reading topic after %s: %v", op.name, err)
				}
				if matching != i+1 {
					t.Fatalf("%q notifications after %s = %d, want %d", c.topic, op.name, matching, i+1)
				}
			}
		})
	}
}

// TestSnapshotPruneNotifiesCascadedDeletes は、GC で消える 3 つの子行に対応する通知を検査する。
func TestSnapshotPruneNotifiesCascadedDeletes(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := t.Context()
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	p := seedReservationProgram(t, db, "prune notification fixture", contracttest.ReservationProgramOptions{
		Snapshot: true, Reservation: true, Overrides: []byte(`{"priority":1}`), StartAt: start,
	})
	execFixture(t, db, "INSERT INTO program_intents (site, program_id, action) VALUES (?, ?, 'record')", p.Site, p.ProgramID)
	execFixture(t, db, "DELETE FROM notifications")
	if n, err := NewReservations(db).PruneEndedSnapshots(ctx, start.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("PruneEndedSnapshots = (%d, %v), want (1, nil)", n, err)
	}
	var matching int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifications WHERE payload = 'reservations'").Scan(&matching); err != nil {
		t.Fatal(err)
	}
	if total := countNotifications(t, db); total != 3 || matching != 3 {
		t.Fatalf("notifications after pruning = %d total and %d reservations, want three reservations notifications", total, matching)
	}
}

// TestApplyPassNotifiesOnlyChangedReservations は、予約を変えた行だけに通知が出ることを検査する。
func TestApplyPassNotifiesOnlyChangedReservations(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := t.Context()
	reservations := NewReservations(db)
	ruleID := fixtureID(t, execFixture(t, db, "INSERT INTO rules (name) VALUES ('ruler notification fixture')"))
	first := seedReservationProgram(t, db, "first reservation", contracttest.ReservationProgramOptions{Snapshot: true})
	second := seedReservationProgram(t, db, "second reservation", contracttest.ReservationProgramOptions{Snapshot: true})
	revision, err := reservations.GetRulerInputRevision(ctx, first.Site)
	if err != nil {
		t.Fatalf("GetRulerInputRevision: %v", err)
	}
	plan := store.RulerPassPlan{
		InputRevision: revision,
		Reservations: []store.RulerReservationInput{
			{ProgramID: first.ProgramID, RuleID: &ruleID, Base: json.RawMessage(`{"priority":1}`)},
			{ProgramID: second.ProgramID, RuleID: &ruleID, Base: json.RawMessage(`{"priority":1}`)},
		},
	}
	applyAndCheckNotifications := func(step string, want int) {
		t.Helper()
		if _, err := reservations.ApplyPass(ctx, first.Site, plan); err != nil {
			t.Fatalf("ApplyPass (%s): %v", step, err)
		}
		var matching int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifications WHERE payload = 'reservations'").Scan(&matching); err != nil {
			t.Fatalf("counting reservations notifications (%s): %v", step, err)
		}
		if matching != want {
			t.Fatalf("reservations notifications after %s = %d, want %d", step, matching, want)
		}
	}
	applyAndCheckNotifications("inserting both reservations", 2)
	applyAndCheckNotifications("repeating the same plan", 2)

	plan.ExistingProgramIDs = []int64{first.ProgramID, second.ProgramID}
	plan.Reservations[0].Base = json.RawMessage(`{"priority":2}`)
	applyAndCheckNotifications("updating one reservation", 3)
}
