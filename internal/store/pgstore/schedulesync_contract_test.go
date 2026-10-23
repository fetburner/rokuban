package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestScheduleSyncContract(t *testing.T) {
	contracttest.RunScheduleSync(t, func(t *testing.T) contracttest.ScheduleSyncFixture {
		pool := testutil.SetupDB(t)
		s := NewScheduleSync(pool)
		return contracttest.ScheduleSyncFixture{
			Writer: s,
			Reader: s,
			SeedSyncEvaluation: func(t *testing.T, site string, programID int64, title string) {
				t.Helper()
				ctx := context.Background()
				q := sqlcgen.New(pool)
				if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
					Site: site, ProgramID: programID, Title: title,
					StartAt: time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), DurationMs: 30_000,
					NetworkID: 100, ServiceID: 200, ChannelType: "GR", Channel: "contract", EventID: 300, ServiceName: "contract",
				}); err != nil {
					t.Fatalf("creating program snapshot for site %q: %v", site, err)
				}
				if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{Site: site, ProgramID: programID}); err != nil {
					t.Fatalf("creating reservation for site %q: %v", site, err)
				}
			},
			SeedStartedRecording: func(t *testing.T, site string, key store.BroadcastEventKey, startedAt time.Time) {
				t.Helper()
				dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
					fixtureParams.Site = site
					fixtureParams.NetworkID = key.NetworkID
					fixtureParams.ServiceID = key.ServiceID
					fixtureParams.EventID = key.EventID
					fixtureParams.ServiceName = "contract"
					fixtureParams.Channel = "contract"
					fixtureParams.Title = "contract"
					fixtureParams.ProgramStartAt = startedAt
					fixtureParams.ProgramDurationMs = 30_000
					fixtureParams.Status = "recording"
					fixtureParams.StartedAt = &startedAt
				})
			},
			SetSnapshotAt: func(t *testing.T, site string, at time.Time) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `UPDATE schedule_sync_snapshots SET snapshot_at = $2 WHERE site = $1`, site, at); err != nil {
					t.Fatalf("setting snapshot marker: %v", err)
				}
			},
			FailSnapshotUpdate: func(t *testing.T) {
				t.Helper()
				ctx := context.Background()
				const functionName = "contract_test_fail_schedule_sync_snapshot"
				const triggerName = "contract_test_fail_schedule_sync_snapshot"
				if _, err := pool.Exec(ctx, `CREATE FUNCTION public.`+functionName+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.site = 'contract-schedule-sync-marker-failure' THEN
    RAISE EXCEPTION 'forced schedule sync snapshot failure';
  END IF;
  RETURN NEW;
END;
$$`); err != nil {
					t.Fatalf("creating marker failure function: %v", err)
				}
				if _, err := pool.Exec(ctx, `CREATE TRIGGER `+triggerName+` BEFORE INSERT OR UPDATE ON schedule_sync_snapshots FOR EACH ROW EXECUTE FUNCTION public.`+functionName+`()`); err != nil {
					t.Fatalf("creating marker failure trigger: %v", err)
				}
				t.Cleanup(func() {
					if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+triggerName+` ON schedule_sync_snapshots`); err != nil {
						t.Errorf("dropping marker failure trigger: %v", err)
					}
					if _, err := pool.Exec(ctx, `DROP FUNCTION IF EXISTS public.`+functionName+`()`); err != nil {
						t.Errorf("dropping marker failure function: %v", err)
					}
				})
			},
		}
	})
}
