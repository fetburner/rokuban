package pgstore

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var rulerPassProgramCounter atomic.Int64
var rulerPassEventCounter atomic.Int32

func TestRulerPassContract(t *testing.T) {
	contracttest.RunRulerPass(t, func(t *testing.T) contracttest.RulerPassFixture {
		pool := testutil.SetupDB(t)
		reservations := NewReservations(pool)
		return contracttest.RulerPassFixture{
			Pass: reservations, Bumper: reservations, Reservations: reservations, Breakers: NewBreakers(pool),
			NewProgram: func(t *testing.T, title string) contracttest.RulerProgram {
				return seedRulerPassProgram(t, pool, title)
			},
			NewRule: func(t *testing.T, name string, dedupeEnabled bool, threshold float32) int64 {
				return seedRulerPassRule(t, pool, name, dedupeEnabled, threshold)
			},
			NewFinishedRecording: func(t *testing.T, ruleID int64, program contracttest.RulerProgram, title string) int64 {
				return seedRulerPassRecording(t, pool, ruleID, program, title)
			},
			SetProgramStarts: func(t *testing.T, program contracttest.RulerProgram, snapshotStart, projectionStart time.Time) {
				setRulerPassProgramStarts(t, pool, program, snapshotStart, projectionStart)
			},
			HasProgramSnapshot: func(t *testing.T, program contracttest.RulerProgram) bool {
				return rulerPassSnapshotExists(t, pool, program)
			},
			GetProgramSnapshotStart: func(t *testing.T, program contracttest.RulerProgram) time.Time {
				return rulerPassSnapshotStart(t, pool, program)
			},
			SetDedupeWindow: func(t *testing.T, ruleID int64, window time.Duration) {
				setRulerPassDedupeWindow(t, pool, ruleID, window)
			},
			SetRecordingStartAt: func(t *testing.T, recordingID int64, start time.Time) {
				setRulerPassRecordingStartAt(t, pool, recordingID, start)
			},
			FailBreakerTrip: func(t *testing.T, name string) {
				installRulerPassBreakerFailure(t, pool, name)
			},
		}
	})
}

func TestLoopMarkersContract(t *testing.T) {
	contracttest.RunLoopMarkers(t, func(t *testing.T) contracttest.LoopMarkerFixture {
		pool := testutil.SetupDB(t)
		return contracttest.LoopMarkerFixture{
			Store: NewReservations(pool),
			FailRulerMarkerForSite: func(t *testing.T, site string) {
				installRulerMarkerFailure(t, pool, site)
			},
		}
	})
}

func seedRulerPassProgram(t *testing.T, pool *pgxpool.Pool, title string) contracttest.RulerProgram {
	t.Helper()
	ctx := context.Background()
	const site = "contract-ruler-pass"
	const networkID = int32(32736)
	const serviceID = int32(5168)
	programID := int64(700000000000000) + rulerPassProgramCounter.Add(1)
	eventID := rulerPassEventCounter.Add(1)
	startAt := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (
  site, network_id, service_id, type, logo_id, remote_control_key_id,
  name, channel_type, channel, has_logo_data
) VALUES ($1, $2, $3, 1, 0, 1, 'ruler pass contract', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, site, networkID, serviceID); err != nil {
		t.Fatalf("seeding ruler pass service: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description
) VALUES ($1, $2, $3, $4, $5, $6::timestamptz, 1800000, $6::timestamptz + interval '30 minutes', true, $7, '')`,
		site, programID, networkID, serviceID, eventID, startAt, title); err != nil {
		t.Fatalf("seeding ruler pass EPG program: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
) VALUES ($1, $2, $3, $4, 1800000, $5, $6, 'GR', '27', $7, 'ruler pass contract')`,
		site, programID, title, startAt, networkID, serviceID, eventID); err != nil {
		t.Fatalf("seeding ruler pass snapshot: %v", err)
	}
	return contracttest.RulerProgram{Site: site, ProgramID: programID, EventID: eventID, Title: title, StartAt: startAt}
}

func seedRulerPassRule(t *testing.T, pool *pgxpool.Pool, name string, dedupeEnabled bool, threshold float32) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO rules (name, dedupe_enabled, dedupe_threshold)
VALUES ($1, $2, CASE WHEN $2 THEN $3::real ELSE NULL::real END)
RETURNING id`, name, dedupeEnabled, threshold).Scan(&id); err != nil {
		t.Fatalf("seeding ruler pass rule: %v", err)
	}
	return id
}

func seedRulerPassRecording(t *testing.T, pool *pgxpool.Pool, ruleID int64, program contracttest.RulerProgram, title string) int64 {
	t.Helper()
	startAt := program.StartAt.Add(-48 * time.Hour)
	eventID := program.EventID + 10000
	return dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.RuleID = &ruleID
		p.Source = "rule"
		p.Status = "finished"
		p.Site = program.Site
		p.NetworkID = 32736
		p.ServiceID = 5168
		p.EventID = eventID
		p.ServiceName = "ruler pass contract"
		p.Title = title
		p.ProgramStartAt = startAt
	})
}

func setRulerPassProgramStarts(t *testing.T, pool *pgxpool.Pool, program contracttest.RulerProgram, snapshotStart, projectionStart time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
UPDATE program_snapshots SET start_at = $3::timestamptz
WHERE site = $1 AND program_id = $2`, program.Site, program.ProgramID, snapshotStart); err != nil {
		t.Fatalf("setting ruler pass snapshot start: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE epg_programs
SET start_at = $3::timestamptz, end_at = $3::timestamptz + interval '30 minutes'
WHERE site = $1 AND program_id = $2`, program.Site, program.ProgramID, projectionStart); err != nil {
		t.Fatalf("setting ruler pass projection start: %v", err)
	}
}

func rulerPassSnapshotExists(t *testing.T, pool *pgxpool.Pool, program contracttest.RulerProgram) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
  SELECT 1 FROM program_snapshots WHERE site = $1 AND program_id = $2
)`, program.Site, program.ProgramID).Scan(&exists); err != nil {
		t.Fatalf("checking ruler pass snapshot: %v", err)
	}
	return exists
}

func rulerPassSnapshotStart(t *testing.T, pool *pgxpool.Pool, program contracttest.RulerProgram) time.Time {
	t.Helper()
	var startAt time.Time
	if err := pool.QueryRow(context.Background(), `
SELECT start_at FROM program_snapshots WHERE site = $1 AND program_id = $2`, program.Site, program.ProgramID).Scan(&startAt); err != nil {
		t.Fatalf("reading ruler pass snapshot start: %v", err)
	}
	return startAt
}

func setRulerPassDedupeWindow(t *testing.T, pool *pgxpool.Pool, ruleID int64, window time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE rules SET dedupe_window = $2::double precision * interval '1 second'
WHERE id = $1`, ruleID, window.Seconds()); err != nil {
		t.Fatalf("setting ruler pass dedupe window: %v", err)
	}
}

func setRulerPassRecordingStartAt(t *testing.T, pool *pgxpool.Pool, recordingID int64, start time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE recordings SET program_start_at = $2::timestamptz WHERE id = $1`, recordingID, start); err != nil {
		t.Fatalf("setting ruler pass recording start: %v", err)
	}
}

func installRulerPassBreakerFailure(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS reject_ruler_pass_breaker ON circuit_breakers`); err != nil {
		t.Fatalf("dropping ruler pass breaker trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION reject_ruler_pass_breaker_write() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.name = TG_ARGV[0] THEN
    RAISE EXCEPTION 'ruler pass breaker write rejected for %', NEW.name;
  END IF;
  RETURN NEW;
END;
$$`); err != nil {
		t.Fatalf("creating ruler pass breaker trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE TRIGGER reject_ruler_pass_breaker
BEFORE INSERT OR UPDATE ON circuit_breakers
FOR EACH ROW EXECUTE FUNCTION reject_ruler_pass_breaker_write(`+sqlLiteral(name)+`)`); err != nil {
		t.Fatalf("creating ruler pass breaker trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS reject_ruler_pass_breaker ON circuit_breakers`); err != nil {
			t.Errorf("dropping ruler pass breaker trigger: %v", err)
		}
		if _, err := pool.Exec(ctx, `DROP FUNCTION IF EXISTS reject_ruler_pass_breaker_write()`); err != nil {
			t.Errorf("dropping ruler pass breaker trigger function: %v", err)
		}
	})
}

func installRulerMarkerFailure(t *testing.T, pool *pgxpool.Pool, site string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS reject_ruler_marker_site ON ruler_pass_snapshots`); err != nil {
		t.Fatalf("dropping ruler marker trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION reject_ruler_marker_site_write() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.site = TG_ARGV[0] THEN
    RAISE EXCEPTION 'ruler marker write rejected for site %', NEW.site;
  END IF;
  RETURN NEW;
END;
$$`); err != nil {
		t.Fatalf("creating ruler marker trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE TRIGGER reject_ruler_marker_site
BEFORE INSERT OR UPDATE ON ruler_pass_snapshots
FOR EACH ROW EXECUTE FUNCTION reject_ruler_marker_site_write(`+sqlLiteral(site)+`)`); err != nil {
		t.Fatalf("creating ruler marker trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS reject_ruler_marker_site ON ruler_pass_snapshots`); err != nil {
			t.Errorf("dropping ruler marker trigger: %v", err)
		}
		if _, err := pool.Exec(ctx, `DROP FUNCTION IF EXISTS reject_ruler_marker_site_write()`); err != nil {
			t.Errorf("dropping ruler marker trigger function: %v", err)
		}
	})
}

// sqlLiteral は DDL（CREATE TRIGGER の引数）に埋める文字列リテラルを返す。
// DDL はバインド変数を取れないので、単引用符を二重にして囲む。
func sqlLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}
