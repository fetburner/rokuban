package pgstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRulesContract(t *testing.T) {
	contracttest.RunRules(t, func(t *testing.T) contracttest.RulesFixture {
		pool := testutil.SetupDB(t)
		rules := NewRules(pool)
		reservations := NewReservations(pool)
		return contracttest.RulesFixture{
			Store: rules, Import: rules, Reservations: reservations,
			SeedReservation: func(t *testing.T, ruleID int64, withIntent bool) (string, int64) {
				t.Helper()
				site, programID := seedReservationProgram(t, pool, contracttest.ReservationProgramOptions{
					Snapshot: true, Reservation: true,
				})
				ctx := context.Background()
				if withIntent {
					if err := reservations.PutIntent(ctx, site, programID, reservation.IntentRecord); err != nil {
						t.Fatalf("writing reservation intent: %v", err)
					}
				}
				if _, err := pool.Exec(ctx, `UPDATE reservations SET rule_id = $3 WHERE site = $1 AND program_id = $2`, site, programID, ruleID); err != nil {
					t.Fatalf("linking reservation to rule: %v", err)
				}
				return site, programID
			},
			HoldRule: func(t *testing.T, ruleID int64) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning rule lock tx: %v", err)
				}
				var lockedID int64
				if err := tx.QueryRow(ctx, `SELECT id FROM rules WHERE id = $1 FOR UPDATE`, ruleID).Scan(&lockedID); err != nil {
					rollback(tx)
					t.Fatalf("holding rule row: %v", err)
				}
				var once sync.Once
				release := func() { once.Do(func() { _ = tx.Commit(ctx) }) }
				t.Cleanup(release)
				return release
			},
			WaitForRuleWriters: func(t *testing.T, n int) {
				t.Helper()
				waitForRuleWriters(t, pool, n)
			},
		}
	})
}

func waitForRuleWriters(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	ctx := context.Background()
	monitor, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connecting lock monitor: %v", err)
	}
	defer func() { _ = monitor.Close(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		err := monitor.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
  AND query ILIKE '%UPDATE rules SET%'`).Scan(&got)
		if err == nil && got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d updates waiting on the rule row lock, want %d", got, want)
}
