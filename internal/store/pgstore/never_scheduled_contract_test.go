package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestNeverScheduledEventsContract(t *testing.T) {
	contracttest.RunNeverScheduled(t, func(t *testing.T) contracttest.NeverScheduledFixture {
		pool := testutil.SetupDB(t)
		return contracttest.NeverScheduledFixture{
			Store: NewNeverScheduledEvents(pool),
			AgeObservation: func(t *testing.T, site string, event store.BroadcastEventKey, age time.Duration) {
				t.Helper()
				if _, err := pool.Exec(context.Background(),
					`UPDATE never_scheduled_events SET observed_at = now() - $5::interval
					 WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4`,
					site, event.NetworkID, event.ServiceID, event.EventID,
					pgtype.Interval{Microseconds: age.Microseconds(), Valid: true}); err != nil {
					t.Fatalf("aging never-scheduled observation: %v", err)
				}
			},
		}
	})
}
