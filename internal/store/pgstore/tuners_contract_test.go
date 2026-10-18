package pgstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestTunersContract(t *testing.T) {
	contracttest.RunTuners(t, func(t *testing.T) contracttest.TunerFixture {
		pool := testutil.SetupDB(t)
		tunerStore := NewTuners(pool)
		return contracttest.TunerFixture{
			Sync:     tunerStore,
			Reader:   tunerStore,
			Capacity: tunerStore,
			SeedTuner: func(t *testing.T, site string, tuner store.TunerInput, observedAt time.Time) {
				t.Helper()
				seedContractTuner(t, pool, site, tuner, observedAt)
			},
			SeedDemand: func(t *testing.T, site string, programID int64, channelType, channel string, startAt time.Time, durationMs int64) {
				t.Helper()
				seedContractDemand(t, pool, site, programID, channelType, channel, startAt, durationMs)
			},
			SeedRule: func(t *testing.T, enabled, dedupeEnabled bool) int64 {
				t.Helper()
				return seedContractRule(t, pool, enabled, dedupeEnabled)
			},
			SeedPreviewProgram: func(t *testing.T, ref store.CapacityProgramRef, channelType, channel string, startAt time.Time, durationMs int64) {
				t.Helper()
				seedContractPreviewProgram(t, pool, ref, channelType, channel, startAt, durationMs)
			},
		}
	})
}

func TestCapacityReadsUseSingleSnapshot(t *testing.T) {
	for _, readAllSites := range []bool{false, true} {
		name := "site"
		if readAllSites {
			name = "all sites"
		}
		t.Run(name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			storage := NewTuners(pool)
			start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Millisecond)
			seedContractDemand(t, pool, "site-a", 7201, "GR", "27", start, time.Minute.Milliseconds())
			seedContractTuner(t, pool, "site-a", store.TunerInput{Index: 0, Name: "old", Types: []string{"GR"}}, start)

			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("beginning concurrent writer: %v", err)
			}
			defer rollback(writer)
			if _, err := writer.Exec(ctx, `LOCK TABLE tuner_sync IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatalf("locking tuner_sync: %v", err)
			}
			if _, err := writer.Exec(ctx, `UPDATE tuner_sync SET is_available = true WHERE site = 'site-a' AND tuner_index = 0`); err != nil {
				t.Fatalf("updating tuner_sync: %v", err)
			}
			if _, err := writer.Exec(ctx, `UPDATE reservations SET base = '{"skip":true}'::jsonb WHERE site = 'site-a' AND program_id = 7201`); err != nil {
				t.Fatalf("updating reservation: %v", err)
			}

			type readResult struct {
				input store.CapacityInput
				err   error
			}
			resultCh := make(chan readResult, 1)
			go func() {
				var input store.CapacityInput
				var err error
				if readAllSites {
					input, err = storage.ReadAllSites(ctx)
				} else {
					input, err = storage.ReadSite(ctx, "site-a")
				}
				resultCh <- readResult{input: input, err: err}
			}()
			if err := waitForTunerReadLock(ctx, pool); err != nil {
				t.Fatalf("waiting for capacity read to reach tuner query: %v", err)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("committing concurrent writer: %v", err)
			}

			var result readResult
			select {
			case result = <-resultCh:
			case <-ctx.Done():
				t.Fatalf("waiting for capacity read: %v", ctx.Err())
			}
			if result.err != nil {
				t.Fatalf("reading capacity snapshot: %v", result.err)
			}
			if len(result.input.Demands) != 1 || string(result.input.Demands[0].Base) != "{}" {
				t.Errorf("snapshot demand = %+v, want pre-update reservation", result.input.Demands)
			}
			if len(result.input.Tuners) != 1 || result.input.Tuners[0].IsAvailable {
				t.Errorf("snapshot tuners = %+v, want pre-update unavailable tuner", result.input.Tuners)
			}
		})
	}
}

func waitForTunerReadLock(ctx context.Context, pool *pgxpool.Pool) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_stat_activity
    WHERE pid <> pg_backend_pid()
      AND datname = current_database()
      AND wait_event_type = 'Lock'
      AND query ILIKE '%FROM tuner_sync%'
)`).Scan(&waiting)
		if err != nil {
			return fmt.Errorf("checking for blocked tuner read: %w", err)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func seedContractTuner(t *testing.T, pool *pgxpool.Pool, site string, tuner store.TunerInput, observedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		site, tuner.Index, tuner.Name, tuner.Types, tuner.IsAvailable, tuner.IsFault, observedAt); err != nil {
		t.Fatalf("seeding tuner_sync: %v", err)
	}
}

func seedContractDemand(t *testing.T, pool *pgxpool.Pool, site string, programID int64, channelType, channel string, startAt time.Time, durationMs int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms, network_id, service_id, channel_type, channel,
  event_id, service_name
) VALUES ($1, $2, 'store contract demand', $3, $4, 32678, 5168, $5, $6, $7, 'test service')`,
		site, programID, startAt, durationMs, channelType, channel, int32(programID%100000)); err != nil {
		t.Fatalf("seeding program snapshot: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base)
VALUES ($1, $2, '{}'::jsonb)`, site, programID); err != nil {
		t.Fatalf("seeding reservation: %v", err)
	}
}

func seedContractRule(t *testing.T, pool *pgxpool.Pool, enabled, dedupeEnabled bool) int64 {
	t.Helper()
	var ruleID int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO rules (name, enabled, dedupe_enabled, dedupe_threshold)
VALUES ('store capacity preview', $1, $2, CASE WHEN $2 THEN 0.7 ELSE NULL END)
RETURNING id`, enabled, dedupeEnabled).Scan(&ruleID); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}
	return ruleID
}

func seedContractPreviewProgram(t *testing.T, pool *pgxpool.Pool, ref store.CapacityProgramRef, channelType, channel string, startAt time.Time, durationMs int64) {
	t.Helper()
	ctx := context.Background()
	serviceID := int32(20_000 + ref.ProgramID%20_000)
	eventID := int32(ref.ProgramID % 100_000)
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (
  site, network_id, service_id, type, logo_id, remote_control_key_id,
  name, channel_type, channel, has_logo_data
) VALUES ($1, 32736, $2, 1, 0, 1, 'store preview service', $3, $4, false)`,
		ref.Site, serviceID, channelType, channel); err != nil {
		t.Fatalf("seeding EPG service: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genres
 ) VALUES ($1, $2, 32736, $3, $4, $5::timestamptz, $6::bigint,
           ($5::timestamptz + ($6::bigint * interval '1 millisecond')),
           true, 'store preview program', '', '[]'::jsonb)`,
		ref.Site, ref.ProgramID, serviceID, eventID, startAt, durationMs); err != nil {
		t.Fatalf("seeding EPG program: %v", err)
	}
}
