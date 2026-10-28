package pgstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRecordObservationsContract(t *testing.T) {
	contracttest.RunRecordObservations(t, func(t *testing.T) contracttest.RecordObservationFixture {
		pool := testutil.SetupDB(t)
		newObservation := func(recordID, status string) store.RecordObservation {
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
		seedRecordSync := func(t *testing.T, observation store.RecordObservation) {
			t.Helper()
			if err := sqlcgen.New(pool).UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
				Site: observation.Site, RecordID: observation.RecordID, ProgramID: observation.ProgramID,
				Status: observation.Status, Tags: observation.Tags, FailedReason: observation.FailedReason,
			}); err != nil {
				t.Fatalf("seeding record_sync: %v", err)
			}
		}
		seedProgramSnapshot := func(t *testing.T, observation store.RecordObservation) {
			t.Helper()
			snapshot := observation.Snapshot
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
    site, program_id, title, start_at, duration_ms,
    network_id, service_id, channel_type, channel, event_id, service_name
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (site, program_id) DO NOTHING`,
				observation.Site, observation.ProgramID, snapshot.Title, snapshot.ProgramStartAt,
				snapshot.ProgramDurationMs, snapshot.NetworkID, snapshot.ServiceID,
				snapshot.ChannelType, snapshot.Channel, snapshot.EventID, snapshot.ServiceName,
			); err != nil {
				t.Fatalf("seeding program snapshot: %v", err)
			}
		}
		seedReservation := func(t *testing.T, observation store.RecordObservation) {
			t.Helper()
			seedProgramSnapshot(t, observation)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id) VALUES ($1, $2)
ON CONFLICT (site, program_id) DO NOTHING`, observation.Site, observation.ProgramID); err != nil {
				t.Fatalf("seeding reservation: %v", err)
			}
		}
		recordSyncStatus := func(t *testing.T, site, recordID string) string {
			t.Helper()
			var status string
			if err := pool.QueryRow(context.Background(), `
SELECT status FROM record_sync WHERE site = $1 AND record_id = $2`, site, recordID).Scan(&status); err != nil {
				t.Fatalf("reading record_sync status: %v", err)
			}
			return status
		}
		setObservedAt := func(t *testing.T, site, recordID string, observedAt time.Time) {
			t.Helper()
			if _, err := pool.Exec(context.Background(),
				`UPDATE record_sync SET observed_at = $3 WHERE site = $1 AND record_id = $2`, site, recordID, observedAt); err != nil {
				t.Fatalf("setting record_sync observed_at: %v", err)
			}
		}
		setAfterAcquired := func(t *testing.T, hook func()) {
			t.Helper()
			afterRecordAcquired = hook
			t.Cleanup(func() { afterRecordAcquired = nil })
		}
		newOriginalAsset := func(t *testing.T, recordingID int64, state string) {
			t.Helper()
			assetID := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
				fixtureParams.RelPath = fmt.Sprintf("sites/contract-site/%d/original.m2ts", recordingID)
			})

			if state != "active" {
				dbtest.SetMediaAssetState(t, pool, assetID, state)
			}
		}
		return contracttest.RecordObservationFixture{
			Store: NewRecordings(pool, dbtest.StoreName), Catalog: NewCatalog(pool, []string{"contract-site"}, dbtest.StoreName),
			Reservations: NewReservations(pool), RulerPass: NewReservations(pool), Rules: NewRules(pool, []string{"contract-site"}),
			IngestTarget: NewRecordings(pool, dbtest.StoreName), Backlog: NewRecordings(pool, dbtest.StoreName), NewObservation: newObservation,
			SeedProgramSnapshot: seedProgramSnapshot, SeedRecordSync: seedRecordSync, SeedReservation: seedReservation,
			NewOriginalAsset: newOriginalAsset, RecordSyncStatus: recordSyncStatus,
			SetRecordSyncObservedAt: setObservedAt, SetAfterRecordAcquired: setAfterAcquired,
		}
	})
}
