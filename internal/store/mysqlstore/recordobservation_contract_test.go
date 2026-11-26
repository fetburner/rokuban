package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestRecordObservationsContract(t *testing.T) {
	t.Parallel()
	contracttest.RunRecordObservations(t, func(t *testing.T) contracttest.RecordObservationFixture {
		db := mysqltest.SetupDB(t)
		reservations := NewReservations(db)
		recordings := NewRecordings(db)
		return contracttest.RecordObservationFixture{
			Store: recordings, Catalog: NewCatalog(db, []string{"contract-site"}, catalogContractStoreName),
			Reservations: reservations, RulerPass: reservations, Rules: NewRules(db, []string{"contract-site"}),
			IngestTarget: NewRecordings(db), Backlog: NewRecordings(db),
			NewObservation: newContractRecordObservation,
			SeedProgramSnapshot: func(t *testing.T, observation store.RecordObservation) {
				t.Helper()
				seedContractProgramSnapshot(t, db, observation.Site, observation.ProgramID, observation.Snapshot)
			},
			SeedRecordSync: func(t *testing.T, observation store.RecordObservation) {
				t.Helper()
				tags, err := json.Marshal(observation.Tags)
				if err != nil {
					t.Fatalf("marshalling tags: %v", err)
				}
				execFixture(t, db, `
INSERT INTO record_sync (site, record_id, recording_id, program_id, status, tags, failed_reason)
VALUES (?, ?, NULL, ?, ?, ?, ?)`,
					observation.Site, observation.RecordID, observation.ProgramID, observation.Status, string(tags), nullableJSON(observation.FailedReason))
			},
			SeedReservation: func(t *testing.T, observation store.RecordObservation) {
				t.Helper()
				seedContractProgramSnapshot(t, db, observation.Site, observation.ProgramID, observation.Snapshot)
				execFixture(t, db, `
INSERT INTO reservations (site, program_id) VALUES (?, ?)
ON DUPLICATE KEY UPDATE program_id = program_id`, observation.Site, observation.ProgramID)
			},
			NewOriginalAsset: func(t *testing.T, recordingID int64, state string) {
				t.Helper()
				res := execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 1000)`, recordingID)
				assetID, err := res.LastInsertId()
				if err != nil {
					t.Fatalf("reading media asset ID: %v", err)
				}
				var deletedAt any
				if state == "deleted" {
					deletedAt = time.Now().UTC()
				}
				execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation, deleted_at)
VALUES (?, ?, ?, ?, 0, ?)`,
					assetID, catalogContractStoreName, fmt.Sprintf("sites/contract-site/%d/original.m2ts", recordingID), state, deletedAt)
			},
			RecordSyncStatus: func(t *testing.T, site, recordID string) string {
				t.Helper()
				var status string
				if err := db.QueryRowContext(context.Background(), `SELECT status FROM record_sync WHERE site = ? AND record_id = ?`, site, recordID).Scan(&status); err != nil {
					t.Fatalf("reading record_sync status: %v", err)
				}
				return status
			},
			SetRecordSyncObservedAt: func(t *testing.T, site, recordID string, observedAt time.Time) {
				t.Helper()
				execFixture(t, db, `UPDATE record_sync SET observed_at = ? WHERE site = ? AND record_id = ?`, observedAt, site, recordID)
			},
			SetAfterRecordAcquired: func(t *testing.T, hook func()) {
				t.Helper()
				recordings.afterRecordAcquired = hook
			},
		}
	})
}

// newContractRecordObservation は契約テストの既定の観測を作る。
func newContractRecordObservation(recordID, status string) store.RecordObservation {
	started := time.Date(2026, time.May, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	return store.RecordObservation{
		Site: "contract-site", RecordID: recordID, ProgramID: 812345,
		Snapshot: store.RecordingSnapshot{
			NetworkID: 32736, ServiceID: 1024, EventID: 808,
			ServiceName: "契約局", ChannelType: "GR", Channel: "27", Title: "契約番組",
			ProgramStartAt: started, ProgramDurationMs: int64(time.Hour / time.Millisecond),
		},
		Managed: true, Tags: []string{"rokuban"}, Status: status,
		StartedAt: &started, EndedAt: &ended,
	}
}

// seedContractProgramSnapshot は番組のスナップショットを 1 行入れる。既にあれば何もしない。
func seedContractProgramSnapshot(t *testing.T, db *sql.DB, site string, programID int64, snapshot store.RecordingSnapshot) {
	t.Helper()
	execFixture(t, db, `
INSERT INTO program_snapshots (
    site, program_id, title, start_at, duration_ms,
    network_id, service_id, channel_type, channel, event_id, service_name
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE program_id = program_id`,
		site, programID, snapshot.Title, snapshot.ProgramStartAt, snapshot.ProgramDurationMs,
		snapshot.NetworkID, snapshot.ServiceID, snapshot.ChannelType, snapshot.Channel, snapshot.EventID, snapshot.ServiceName)
}
