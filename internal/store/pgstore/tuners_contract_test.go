package pgstore

import (
	"context"
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
			SeedRule: func(t *testing.T, ruleID int64, enabled, dedupeEnabled bool) {
				t.Helper()
				seedContractRule(t, pool, ruleID, enabled, dedupeEnabled)
			},
			SeedPreviewProgram: func(t *testing.T, ref store.CapacityProgramRef, channelType, channel string, startAt time.Time, durationMs int64) {
				t.Helper()
				seedContractPreviewProgram(t, pool, ref, channelType, channel, startAt, durationMs)
			},
		}
	})
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

func seedContractRule(t *testing.T, pool *pgxpool.Pool, ruleID int64, enabled, dedupeEnabled bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO rules (id, name, enabled, dedupe_enabled, dedupe_threshold)
VALUES ($1, 'store capacity preview', $2, $3, CASE WHEN $3 THEN 0.7 ELSE NULL END)`,
		ruleID, enabled, dedupeEnabled); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}
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
) VALUES ($1, $2, 32736, $3, $4, $5, $6, $5 + ($6 * interval '1 millisecond'), true, 'store preview program', '', '[]'::jsonb)`,
		ref.Site, ref.ProgramID, serviceID, eventID, startAt, durationMs); err != nil {
		t.Fatalf("seeding EPG program: %v", err)
	}
}
