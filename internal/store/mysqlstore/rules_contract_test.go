package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestRulesContract(t *testing.T) {
	t.Parallel()
	contracttest.RunRules(t, func(t *testing.T) contracttest.RulesFixture {
		db := mysqltest.SetupDB(t)
		allSites := []string{"rev-a", "rev-b", "rev-c"}
		rules := NewRules(db, allSites)
		reservations := NewReservations(db)
		return contracttest.RulesFixture{
			Store: rules, Import: rules, Reservations: reservations, Pass: reservations, AllSites: allSites,
			SeedReservation: func(t *testing.T, ruleID int64, withIntent bool) (string, int64) {
				t.Helper()
				p := seedReservationProgram(t, db, "rules contract", contracttest.ReservationProgramOptions{
					Snapshot: true, Reservation: true,
				})
				ctx := context.Background()
				if withIntent {
					if err := reservations.PutIntent(ctx, p.Site, p.ProgramID, reservation.IntentRecord); err != nil {
						t.Fatalf("writing reservation intent: %v", err)
					}
				}
				execFixture(t, db, `UPDATE reservations SET rule_id = ? WHERE site = ? AND program_id = ?`, ruleID, p.Site, p.ProgramID)
				return p.Site, p.ProgramID
			},
			HoldRule: func(t *testing.T, ruleID int64) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning rule lock tx: %v", err)
				}
				var locked int64
				if err := tx.QueryRowContext(ctx, `SELECT id FROM rules WHERE id = ? FOR UPDATE`, ruleID).Scan(&locked); err != nil {
					_ = tx.Rollback()
					t.Fatalf("holding rule row: %v", err)
				}
				var once sync.Once
				release := func() {
					once.Do(func() {
						if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
							t.Errorf("releasing rule hold: %v", err)
						}
					})
				}
				t.Cleanup(release)
				return release
			},
			FailRuleDeletion: func(t *testing.T) func() {
				t.Helper()
				execFixture(t, db, `
CREATE TRIGGER fail_rule_delete BEFORE DELETE ON rules FOR EACH ROW
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced rule deletion failure'`)
				var once sync.Once
				restore := func() {
					once.Do(func() {
						execFixture(t, db, `DROP TRIGGER fail_rule_delete`)
					})
				}
				t.Cleanup(restore)
				return restore
			},
			WaitForRuleWriters: func(t *testing.T, n int) {
				t.Helper()
				waitLockWaiters(t, db, "rules", n)
			},
			SeedRecording: func(t *testing.T, ruleID int64) int64 {
				return seedRuleRecording(t, db, ruleID)
			},
			RecordingRuleID: func(t *testing.T, recordingID int64) *int64 {
				t.Helper()
				var ruleID sql.NullInt64
				if err := db.QueryRowContext(context.Background(), `SELECT rule_id FROM recordings WHERE id = ?`, recordingID).Scan(&ruleID); err != nil {
					t.Fatalf("reading recording %d: %v", recordingID, err)
				}
				if !ruleID.Valid {
					return nil
				}
				return &ruleID.Int64
			},
		}
	})
}

// seedRuleRecording は、このテスト DB にルール由来の録画を 1 件作る。
func seedRuleRecording(t *testing.T, db *sql.DB, ruleID int64) int64 {
	t.Helper()
	return fixtureID(t, execFixture(t, db, `
INSERT INTO recordings (rule_id, source, site, network_id, service_id, event_id, service_name, channel_type, channel,
                        title, program_start_at, program_duration_ms, status)
VALUES (?, 'rule', 'rules-contract', 1, 1, 1, 'store contract', 'GR', '27', 'recording', CURRENT_TIMESTAMP(6), 1800000, 'finished')
`, ruleID))
}

// TestRuleDeleteNotifiesRecordings は、録画の参照を外す更新が recordings の通知を出すことを確かめる。
// FK の ON DELETE SET NULL だけでは参照は外れるが、トリガーが発火せず通知が出ない。
func TestRuleDeleteNotifiesRecordings(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	rules := NewRules(db, []string{"rules-contract"})
	rule, err := rules.Create(ctx, store.RuleInput{Name: "notify"})
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	seedRuleRecording(t, db, rule.ID)
	countRecordingNotifications := func() int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE payload = 'recordings'`).Scan(&count); err != nil {
			t.Fatalf("counting recording notifications: %v", err)
		}
		return count
	}
	before := countRecordingNotifications()
	if _, err := rules.Delete(ctx, rule.ID); err != nil {
		t.Fatalf("Delete(rule) error = %v", err)
	}
	if after := countRecordingNotifications(); after <= before {
		t.Fatalf("recording notifications after Delete = %d, want more than %d", after, before)
	}
}
