package pgstore

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var reservationProgramID atomic.Int64

func TestReservationsContract(t *testing.T) {
	contracttest.RunReservationsContract(t, func(t *testing.T) contracttest.ReservationsFixture {
		pool := testutil.SetupDB(t)
		return contracttest.ReservationsFixture{
			Store: NewReservations(pool),
			NewProgram: func(t *testing.T, options contracttest.ReservationProgramOptions) (string, int64) {
				return seedReservationProgram(t, pool, options)
			},
		}
	})
}

func TestPatchOverridesLocksSnapshotBeforeReadingExistingOverrides(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	monitor, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connecting lock monitor: %v", err)
	}
	t.Cleanup(func() { _ = monitor.Close(ctx) })
	site, programID := seedReservationProgram(t, pool, contracttest.ReservationProgramOptions{
		Projection: true, Snapshot: true, Reservation: true, Overrides: json.RawMessage(`{"priority":7}`),
	})
	s := NewReservations(pool)

	snapshotBlocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning snapshot lock: %v", err)
	}
	defer rollback(snapshotBlocker)
	if err := snapshotBlocker.QueryRow(ctx,
		`SELECT true FROM program_snapshots WHERE site = $1 AND program_id = $2 FOR UPDATE`, site, programID).Scan(new(bool)); err != nil {
		t.Fatalf("locking program snapshot: %v", err)
	}
	overridesBlocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning overrides lock: %v", err)
	}
	defer rollback(overridesBlocker)
	if err := overridesBlocker.QueryRow(ctx,
		`SELECT true FROM program_overrides WHERE site = $1 AND program_id = $2 FOR UPDATE`, site, programID).Scan(new(bool)); err != nil {
		t.Fatalf("locking existing program overrides: %v", err)
	}

	contentPath := "recordings/concurrent.ts"
	filenameTemplate := "{{.Title}}"
	patches := []store.OverridesPatch{{ContentPath: &contentPath}, {FilenameTemplate: &filenameTemplate}}
	results := make(chan error, len(patches))
	for _, patch := range patches {
		patch := patch
		go func() { results <- s.PatchOverrides(ctx, site, programID, patch) }()
	}
	waitForSnapshotPatchWaiters(t, monitor, len(patches))
	if err := snapshotBlocker.Commit(ctx); err != nil {
		t.Fatalf("releasing program snapshot lock: %v", err)
	}
	waitForPatchWaiters(t, monitor, 1, 1)
	if err := overridesBlocker.Commit(ctx); err != nil {
		t.Fatalf("releasing existing overrides lock: %v", err)
	}
	for range patches {
		if err := <-results; err != nil {
			t.Fatalf("PatchOverrides() = %v, want success", err)
		}
	}

	row, err := s.GetReservation(ctx, site, programID)
	if err != nil {
		t.Fatalf("reading reservation after competing patches: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(row.Overrides, &got); err != nil {
		t.Fatalf("decoding overrides %s: %v", row.Overrides, err)
	}
	if got["contentPath"] != contentPath || got["filenameTemplate"] != filenameTemplate || got["priority"] != float64(7) {
		t.Fatalf("overrides after competing patches = %s, want both fields and the original priority", row.Overrides)
	}
}

func waitForSnapshotPatchWaiters(t *testing.T, monitor *pgx.Conn, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshotWaiters, _, err := countPatchWaiters(monitor)
		if err == nil && snapshotWaiters >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d patches blocked on the program snapshot", want)
}

func waitForPatchWaiters(t *testing.T, monitor *pgx.Conn, wantSnapshots, wantOverrides int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var snapshotWaiters, overridesWaiters int
	for time.Now().Before(deadline) {
		var err error
		snapshotWaiters, overridesWaiters, err = countPatchWaiters(monitor)
		if err == nil && snapshotWaiters == wantSnapshots && overridesWaiters == wantOverrides {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for blocked patch state snapshots=%d overrides=%d; got snapshots=%d overrides=%d",
		wantSnapshots, wantOverrides, snapshotWaiters, overridesWaiters)
}

func countPatchWaiters(monitor *pgx.Conn) (int, int, error) {
	var snapshotWaiters, overridesWaiters int
	err := monitor.QueryRow(context.Background(), `
SELECT count(*) FILTER (WHERE query ILIKE '%program_snapshots%'),
       count(*) FILTER (WHERE query ILIKE '%program_overrides%')
FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
`).Scan(&snapshotWaiters, &overridesWaiters)
	return snapshotWaiters, overridesWaiters, err
}

func seedReservationProgram(t *testing.T, pool *pgxpool.Pool, options contracttest.ReservationProgramOptions) (string, int64) {
	t.Helper()
	ctx := context.Background()
	const site = db.DefaultSite
	programID := int64(700000000000000) + reservationProgramID.Add(1)
	eventID := int32(reservationProgramID.Load())
	startAt := options.StartAt
	if startAt.IsZero() {
		startAt = time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	}
	endAt := startAt.Add(30 * time.Minute)

	if options.Projection {
		if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, 32678, 5168, 1, 0, 1, 'store contract', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, site); err != nil {
			t.Fatalf("seeding EPG service: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, is_free, name, description)
VALUES ($1, $2, 32678, 5168, $3, $4, 1800000, $5, true, 'store contract program', '')`,
			site, programID, eventID, startAt, endAt); err != nil {
			t.Fatalf("seeding EPG program: %v", err)
		}
	}

	q := sqlcgen.New(pool)
	if options.Snapshot {
		if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
			Site: site, ProgramID: programID, Title: "store contract program", StartAt: startAt,
			DurationMs: 1800000, NetworkID: 32678, ServiceID: 5168, ChannelType: "GR",
			Channel: "27", EventID: eventID, ServiceName: "store contract",
		}); err != nil {
			t.Fatalf("seeding program snapshot: %v", err)
		}
	}
	if options.Reservation {
		if !options.Snapshot {
			t.Fatal("reservation fixture requires a program snapshot")
		}
		if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{Site: site, ProgramID: programID}); err != nil {
			t.Fatalf("seeding reservation: %v", err)
		}
	}
	if options.Overrides != nil {
		if !options.Snapshot {
			t.Fatal("override fixture requires a program snapshot")
		}
		if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
			Site: site, ProgramID: programID, Overrides: options.Overrides,
		}); err != nil {
			t.Fatalf("seeding overrides: %v", err)
		}
	}
	return site, programID
}
