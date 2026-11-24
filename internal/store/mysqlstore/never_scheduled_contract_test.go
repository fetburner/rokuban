package mysqlstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestNeverScheduledEventsContract(t *testing.T) {
	contracttest.RunNeverScheduled(t, func(t *testing.T) contracttest.NeverScheduledFixture {
		db := mysqltest.SetupDB(t)
		return contracttest.NeverScheduledFixture{
			Store: NewNeverScheduledEvents(db),
			AgeObservation: func(t *testing.T, site string, event store.BroadcastEventKey, programStartAt time.Time, age time.Duration) {
				t.Helper()
				if _, err := db.ExecContext(context.Background(), `
					UPDATE never_scheduled_events SET observed_at = NOW(6) - INTERVAL ? MICROSECOND
					WHERE site = ? AND network_id = ? AND service_id = ? AND event_id = ?
					  AND program_start_at = ?
				`, age.Microseconds(), site, event.NetworkID, event.ServiceID, event.EventID, programStartAt); err != nil {
					t.Fatalf("aging never-scheduled observation: %v", err)
				}
			},
		}
	})
}
