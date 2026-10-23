package pgstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var cmStationContractEventID atomic.Int32

func TestCMStationContract(t *testing.T) {
	contracttest.RunCMStation(t, func(t *testing.T) contracttest.CMStationFixture {
		pool := testutil.SetupDB(t)
		newRecording := func(t *testing.T) int64 {
			t.Helper()
			id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
				fixtureParams.EventID = cmStationContractEventID.Add(1)
				fixtureParams.ServiceName = "局単位 CM 契約 fixture"
				fixtureParams.Title = "CM station store contract fixture"
				fixtureParams.ProgramStartAt = time.Now().UTC().Truncate(time.Second)
			})

			dbtest.OriginalAsset(t, pool, id, func(fixtureParams *dbtest.MediaAssetParams) {
				fixtureParams.RelPath = fmt.Sprintf("test/%d.ts", id)
				fixtureParams.SizeBytes = 1000
			})
			return id
		}
		return contracttest.CMStationFixture{
			Store:        NewAnalysis(pool),
			NewRecording: newRecording,
			BeginStationWrite: func(t *testing.T, station store.CMStationRef) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning held station write: %v", err)
				}
				committed := false
				t.Cleanup(func() {
					if !committed {
						_ = tx.Rollback(ctx)
					}
				})
				if err := sqlcgen.New(tx).LockCMStation(ctx, sqlcgen.LockCMStationParams{
					NetworkID: station.NetworkID, ServiceID: station.ServiceID,
				}); err != nil {
					t.Fatalf("locking station: %v", err)
				}
				return func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("committing held station write: %v", err)
					}
					committed = true
				}
			},
			WaitForStationWriteWait: func(t *testing.T, station store.CMStationRef, name string) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var waiting bool
					err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity a
    JOIN pg_locks l ON l.pid = a.pid
    WHERE a.datname = current_database()
      AND a.pid <> pg_backend_pid()
      AND a.wait_event_type = 'Lock'
      AND a.query LIKE '%pg_advisory_xact_lock%'
      AND l.locktype = 'advisory'
      AND NOT l.granted
      AND l.classid = $1::oid
      AND l.objid = $2::oid
      AND l.objsubid = 2
)`, station.NetworkID, station.ServiceID).Scan(&waiting)
					if err != nil {
						t.Fatalf("checking for a wait on the station lock: %v", err)
					}
					if waiting {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatalf("%s did not wait on the held station lock", name)
			},
			SeedCandidate: func(t *testing.T, station store.CMStationRef, recordingID int64, areaUpdatedAt, attemptedAt time.Time, state string) {
				t.Helper()
				var lgd []byte
				if state == "ready" {
					lgd = []byte("candidate logo")
				}
				_, err := pool.Exec(context.Background(), `
INSERT INTO cm_logo_candidates (
    network_id, service_id, state, x, y, w, h, coded_width, coded_height,
    recording_id, observed_area_updated_at, attempted_at, lgd
) VALUES ($1, $2, $3, 10, 10, 100, 50, 1920, 1080, $4, $5, $6, $7)`,
					station.NetworkID, station.ServiceID, state, recordingID, areaUpdatedAt, attemptedAt, lgd)
				if err != nil {
					t.Fatalf("seeding candidate: %v", err)
				}
			},
			FailCandidateDelete: func(t *testing.T, station store.CMStationRef) func() {
				t.Helper()
				const functionName = "cmstation_contract_reject_candidate_delete"
				const triggerName = "cmstation_contract_reject_candidate_delete"
				if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.network_id = %d AND OLD.service_id = %d THEN
        RAISE EXCEPTION 'injected candidate deletion failure';
    END IF;
    RETURN OLD;
END;
$$`, functionName, station.NetworkID, station.ServiceID)); err != nil {
					t.Fatalf("creating candidate delete failure function: %v", err)
				}
				if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
CREATE TRIGGER %s BEFORE DELETE ON cm_logo_candidates
FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName)); err != nil {
					t.Fatalf("creating candidate delete failure trigger: %v", err)
				}
				return func() {
					if _, err := pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER %s ON cm_logo_candidates`, triggerName)); err != nil {
						t.Errorf("dropping candidate delete failure trigger: %v", err)
					}
					if _, err := pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION %s()`, functionName)); err != nil {
						t.Errorf("dropping candidate delete failure function: %v", err)
					}
				}
			},
		}
	})
}
