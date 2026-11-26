package mysqlstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestScheduleSyncContract(t *testing.T) {
	t.Parallel()
	contracttest.RunScheduleSync(t, func(t *testing.T) contracttest.ScheduleSyncFixture {
		db := mysqltest.SetupDB(t)
		s := NewScheduleSync(db)
		return contracttest.ScheduleSyncFixture{
			Writer: s,
			Reader: s,
			SeedSyncEvaluation: func(t *testing.T, site string, programID int64, title string) {
				t.Helper()
				startAt := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
				eventID := programID % 100_000
				execFixture(t, db, `
INSERT INTO program_snapshots (site, program_id, network_id, service_id, event_id, start_at, duration_ms,
                               title, channel_type, channel, service_name)
VALUES (?, ?, ?, ?, ?, ?, 30000, ?, 'GR', 'contract', 'contract')`,
					site, programID, fixtureNetworkID, fixtureServiceID, eventID, startAt, title)
				execFixture(t, db, `INSERT INTO reservations (site, program_id) VALUES (?, ?)`, site, programID)
			},
			SeedStartedRecording: func(t *testing.T, site string, key store.BroadcastEventKey, startedAt time.Time) {
				t.Helper()
				execFixture(t, db, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
                        title, program_start_at, program_duration_ms, status, started_at)
VALUES ('manual', ?, ?, ?, ?, 'contract', 'GR', 'contract', 'contract', ?, 30000, 'recording', ?)`,
					site, key.NetworkID, key.ServiceID, key.EventID, startedAt, startedAt)
			},
			SetSnapshotAt: func(t *testing.T, site string, at time.Time) {
				t.Helper()
				execFixture(t, db, `UPDATE schedule_sync_snapshots SET snapshot_at = ? WHERE site = ?`, at, site)
			},
			FailSnapshotUpdate: func(t *testing.T) {
				t.Helper()
				// 失敗の形は書き込みの側に作る。marker の INSERT も ON DUPLICATE KEY UPDATE の UPDATE も止める。
				for _, timing := range []string{"INSERT", "UPDATE"} {
					name := "contract_fail_schedule_sync_snapshot_" + strings.ToLower(timing)
					if _, err := db.ExecContext(context.Background(), `
						CREATE TRIGGER `+name+` BEFORE `+timing+` ON schedule_sync_snapshots FOR EACH ROW
						BEGIN
							IF NEW.site = 'contract-schedule-sync-marker-failure' THEN
								SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced schedule sync snapshot failure';
							END IF;
						END
					`); err != nil {
						t.Fatalf("creating schedule sync snapshot %s trigger: %v", timing, err)
					}
					t.Cleanup(func() {
						if _, err := db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS "+name); err != nil {
							t.Errorf("dropping schedule sync snapshot %s trigger: %v", timing, err)
						}
					})
				}
			},
		}
	})
}
