package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db"
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
		seedReservation := func(t *testing.T, observation store.RecordObservation) {
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
			if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id) VALUES ($1, $2)
ON CONFLICT (site, program_id) DO NOTHING`, observation.Site, observation.ProgramID); err != nil {
				t.Fatalf("seeding reservation: %v", err)
			}
		}
		inspectRecordSync := func(t *testing.T, site, recordID string) contracttest.RecordSyncState {
			t.Helper()
			var got contracttest.RecordSyncState
			err := pool.QueryRow(context.Background(), `
SELECT recording_id, status FROM record_sync WHERE site = $1 AND record_id = $2`, site, recordID).Scan(&got.RecordingID, &got.Status)
			if errors.Is(err, pgx.ErrNoRows) {
				return got
			}
			if err != nil {
				t.Fatalf("inspecting record_sync: %v", err)
			}
			got.Found = true
			return got
		}
		inspectRecordings := func(t *testing.T, observation store.RecordObservation) []contracttest.ObservedRecording {
			t.Helper()
			rows, err := pool.Query(context.Background(), `
SELECT id, status, superseded_at, quality_events
FROM recordings
WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4 AND program_start_at = $5`,
				observation.Site, observation.Snapshot.NetworkID, observation.Snapshot.ServiceID,
				observation.Snapshot.EventID, observation.Snapshot.ProgramStartAt)
			if err != nil {
				t.Fatalf("querying event recordings: %v", err)
			}
			defer rows.Close()
			var got []contracttest.ObservedRecording
			for rows.Next() {
				var row contracttest.ObservedRecording
				var supersededAt *time.Time
				var qualityEvents json.RawMessage
				if err := rows.Scan(&row.ID, &row.Status, &supersededAt, &qualityEvents); err != nil {
					t.Fatalf("scanning event recording: %v", err)
				}
				row.Superseded = supersededAt != nil
				var events []db.QualityEvent
				if err := json.Unmarshal(qualityEvents, &events); err != nil {
					t.Fatalf("decoding quality events: %v", err)
				}
				row.QualityEventCount = len(events)
				for _, event := range events {
					row.QualityEventTimes = append(row.QualityEventTimes, event.At)
				}
				got = append(got, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("reading event recordings: %v", err)
			}
			return got
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
				if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET state = $1 WHERE id = $2`, state, assetID); err != nil {
					t.Fatalf("setting original asset state to %q: %v", state, err)
				}
			}
		}
		return contracttest.RecordObservationFixture{
			Store: NewRecordings(pool), IngestTarget: NewRecordings(pool), Backlog: NewRecordings(pool), NewObservation: newObservation,
			SeedRecordSync: seedRecordSync, SeedReservation: seedReservation,
			NewOriginalAsset:  newOriginalAsset,
			InspectRecordSync: inspectRecordSync, InspectRecordings: inspectRecordings,
			SetRecordSyncObservedAt: setObservedAt, SetAfterRecordAcquired: setAfterAcquired,
		}
	})
}
