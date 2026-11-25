package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

var labelRuleContractEventID atomic.Int32

func TestLabelRulesContract(t *testing.T) {
	contracttest.RunLabelRules(t, func(t *testing.T) contracttest.LabelRuleFixture {
		db := mysqltest.SetupDB(t)
		labelRules := NewLabelRules(db)
		installLabelRuleHitProbe(t, db)
		return contracttest.LabelRuleFixture{
			Store:        labelRules,
			Reevaluation: labelRules,
			NewRecording: func(t *testing.T, title string) int64 {
				t.Helper()
				return seedLabelRuleRecording(t, db, title)
			},
			BeginReevaluationLock: func(t *testing.T) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning re-evaluation lock tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				if err := lockLabelRuleReconcile(ctx, tx); err != nil {
					t.Fatalf("acquiring re-evaluation lock: %v", err)
				}
				return func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("committing re-evaluation lock tx: %v", err)
					}
				}
			},
			WaitForReevaluationLock: func(t *testing.T) {
				t.Helper()
				waitLockWaiters(t, db, "named_locks", 1)
			},
			LabelRuleHit: func(t *testing.T, recordingID int64) (ruleID int64, rowVersion string, exists bool) {
				t.Helper()
				// 行バージョンは probe（installLabelRuleHitProbe）が数えた書き込みの回数で代える。
				var writes int64
				err := db.QueryRowContext(context.Background(), `
SELECT h.label_rule_id, (SELECT COUNT(*) FROM label_rule_hit_writes w WHERE w.recording_id = h.recording_id)
FROM label_rule_hits h
WHERE h.recording_id = ?`, recordingID).Scan(&ruleID, &writes)
				if errors.Is(err, sql.ErrNoRows) {
					return 0, "", false
				}
				if err != nil {
					t.Fatalf("reading label rule hit for recording %d: %v", recordingID, err)
				}
				return ruleID, strconv.FormatInt(writes, 10), true
			},
			TakeRecordingsNotifications: func(t *testing.T) int {
				t.Helper()
				result := execFixture(t, db, `DELETE FROM notifications WHERE payload = 'recordings'`)
				n, err := result.RowsAffected()
				if err != nil {
					t.Fatalf("counting recordings notifications: %v", err)
				}
				return int(n)
			},
		}
	})
}

// seedLabelRuleRecording は title の録画を 1 件入れ、その ID を返す。
// 録画の INSERT で当たりを作るトリガーも、この関数を通じて動く。
func seedLabelRuleRecording(t *testing.T, db *sql.DB, title string) int64 {
	t.Helper()
	return fixtureID(t, execFixture(t, db, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, program_start_at, program_duration_ms, status)
VALUES ('manual', 'label-rules', 1, 1, ?, 'service', 'GR', 'channel', ?, '2026-01-01 00:00:00', 1000, 'recording')`,
		labelRuleContractEventID.Add(1), title))
}

// installLabelRuleHitProbe は label_rule_hits への書き込みを数える表とトリガーを置く。
// 行バージョンを持たない MySQL で、同じ当たりを書き直していないことを確かめるためである。
func installLabelRuleHitProbe(t *testing.T, db *sql.DB) {
	t.Helper()
	execFixture(t, db, `CREATE TABLE label_rule_hit_writes (
    seq BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    recording_id BIGINT NOT NULL,
    KEY label_rule_hit_writes_recording (recording_id)
) ENGINE=InnoDB`)
	execFixture(t, db, `CREATE TRIGGER label_rule_hits_probe_insert AFTER INSERT ON label_rule_hits FOR EACH ROW
    INSERT INTO label_rule_hit_writes (recording_id) VALUES (NEW.recording_id)`)
	execFixture(t, db, `CREATE TRIGGER label_rule_hits_probe_update AFTER UPDATE ON label_rule_hits FOR EACH ROW
    INSERT INTO label_rule_hit_writes (recording_id) VALUES (NEW.recording_id)`)
	execFixture(t, db, `CREATE TRIGGER label_rule_hits_probe_delete AFTER DELETE ON label_rule_hits FOR EACH ROW
    INSERT INTO label_rule_hit_writes (recording_id) VALUES (OLD.recording_id)`)
}
