package pgstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var reservationProgramID atomic.Int64

func TestReservationsContract(t *testing.T) {
	contracttest.RunReservations(t, func(t *testing.T) contracttest.ReservationsFixture {
		pool := testutil.SetupDB(t)
		return contracttest.ReservationsFixture{
			Store: NewReservations(pool),
			NewProgram: func(t *testing.T, options contracttest.ReservationProgramOptions) (string, int64) {
				return seedReservationProgram(t, pool, options)
			},
			HoldProgram: func(t *testing.T, site string, programID int64) func() {
				return holdProgramSnapshot(t, pool, site, programID)
			},
			WaitBlocked: func(t *testing.T, n int) {
				waitSnapshotWaiters(t, pool, n)
			},
		}
	})
}

// holdProgramSnapshot は、別 tx で番組のスナップショットの行ロックを握る。PutIntent / PatchOverrides が
// 直列化に使う行と同じなので、握っている間、同じ番組の書き込みはそこで待つ。
func holdProgramSnapshot(t *testing.T, pool *pgxpool.Pool, site string, programID int64) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning snapshot hold: %v", err)
	}
	if err := lockProgramSnapshot(ctx, sqlcgen.New(tx), site, programID); err != nil {
		rollback(tx)
		t.Fatalf("holding program snapshot: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Commit(ctx) }) }
	t.Cleanup(release)
	return release
}

// waitSnapshotWaiters は、スナップショットの行ロックを待つ書き込みが n 本になるまで待つ。
// 待ちは pg_stat_activity の Lock 待ちのうち、program_snapshots を触る文を数えて判定する。
func waitSnapshotWaiters(t *testing.T, pool *pgxpool.Pool, want int) {
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
WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%program_snapshots%'`).Scan(&got)
		if err == nil && got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d writes waiting on the program snapshot lock, want %d: the writes did not serialize on it", got, want)
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
