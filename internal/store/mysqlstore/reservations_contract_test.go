package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestReservationsContract(t *testing.T) {
	t.Parallel()
	contracttest.RunReservations(t, func(t *testing.T) contracttest.ReservationsFixture {
		db := mysqltest.SetupDB(t)
		reservations := NewReservations(db)
		return contracttest.ReservationsFixture{
			Store: reservations,
			Pass:  reservations,
			NewProgram: func(t *testing.T, opts contracttest.ReservationProgramOptions) (string, int64) {
				p := seedReservationProgram(t, db, "store contract program", opts)
				return p.Site, p.ProgramID
			},
			HoldProgram: func(t *testing.T, site string, programID int64) func() {
				return holdProgramSnapshot(t, db, site, programID)
			},
			WaitBlocked: func(t *testing.T, n int) {
				waitSnapshotWaiters(t, db, n)
			},
		}
	})
}

const fixtureNetworkID = 32736
const fixtureServiceID = 5168

var fixtureProgramSequence atomic.Int32

func seedReservationProgram(t *testing.T, db *sql.DB, title string, opts contracttest.ReservationProgramOptions) contracttest.RulerProgram {
	t.Helper()
	eventID := fixtureProgramSequence.Add(1)
	programID := int64(700000000000000) + int64(eventID)
	startAt := opts.StartAt
	if startAt.IsZero() {
		startAt = time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	}
	site := opts.Site
	if site == "" {
		site = store.DefaultSite
	}
	if opts.Projection {
		execFixture(t, db, `
  INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel)
  VALUES (?, ?, ?, 1, 0, 1, 'store contract', 'GR', '27')
  ON DUPLICATE KEY UPDATE observed_at = observed_at
  `, site, fixtureNetworkID, fixtureServiceID)
		execFixture(t, db, `
  INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, name)
  VALUES (?, ?, ?, ?, ?, ?, 1800000, ?, ?)
  `, site, programID, fixtureNetworkID, fixtureServiceID, eventID, startAt, startAt.Add(30*time.Minute), title)
	}
	if opts.Snapshot {
		execFixture(t, db, `
  INSERT INTO program_snapshots (site, program_id, network_id, service_id, event_id, start_at, duration_ms, title, channel_type, channel, service_name)
  VALUES (?, ?, ?, ?, ?, ?, 1800000, ?, 'GR', '27', 'store contract')
  `, site, programID, fixtureNetworkID, fixtureServiceID, eventID, startAt, title)
	}
	if opts.Reservation {
		execFixture(t, db, `INSERT INTO reservations (site, program_id) VALUES (?, ?)`, site, programID)
	}
	if opts.Base != nil {
		if !opts.Reservation {
			t.Fatal("base fixture requires a reservation")
		}
		execFixture(t, db, `UPDATE reservations SET base = CAST(? AS JSON) WHERE site = ? AND program_id = ?`, string(opts.Base), site, programID)
	}
	if opts.NeverScheduled {
		if !opts.Snapshot {
			t.Fatal("never-scheduled fixture requires a program snapshot")
		}
		execFixture(t, db, `
  INSERT INTO never_scheduled_events (site, network_id, service_id, event_id, program_start_at)
  VALUES (?, ?, ?, ?, ?)
  `, site, fixtureNetworkID, fixtureServiceID, eventID, startAt)
	}
	if opts.Overrides != nil {
		execFixture(t, db, `INSERT INTO program_overrides (site, program_id, overrides) VALUES (?, ?, ?)`, site, programID, string(opts.Overrides))
	}
	return contracttest.RulerProgram{Site: site, ProgramID: programID, EventID: eventID, Title: title, StartAt: startAt}
}

// holdProgramSnapshot は PutIntent と PatchOverrides が直列化する番組行を別 tx でロックする。
func holdProgramSnapshot(t *testing.T, db *sql.DB, site string, programID int64) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning snapshot hold: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Errorf("releasing snapshot hold: %v", err)
			}
		})
	}
	t.Cleanup(release)
	var id int64
	if err := tx.QueryRowContext(ctx, `
 SELECT program_id FROM program_snapshots WHERE site = ? AND program_id = ? FOR UPDATE
 `, site, programID).Scan(&id); err != nil {
		t.Fatalf("holding program snapshot: %v", err)
	}
	return release
}

// waitSnapshotWaiters はこのテスト DB の program_snapshots で待機する tx を数える。
func waitSnapshotWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	waitLockWaiters(t, db, "program_snapshots", want)
}

// waitLockWaiters はこのテスト DB の table の行ロックで待機する tx が want 本になるまで待つ。
// performance_schema.data_lock_waits を読むので、待機は実際にロック待ちでなければ数えられない。
func waitLockWaiters(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got int
	for {
		err := db.QueryRowContext(ctx, `
  SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
  FROM performance_schema.data_lock_waits w
  JOIN performance_schema.data_locks l
    ON l.ENGINE = w.ENGINE AND l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
  WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?
  `, table).Scan(&got)
		if err != nil {
			t.Fatalf("observing %s lock waiters (%d observed, want %d): %v", table, got, want, err)
		}
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
