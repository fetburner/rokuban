package mysqlstore

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestTunersContract(t *testing.T) {
	t.Parallel()
	contracttest.RunTuners(t, func(t *testing.T) contracttest.TunerFixture {
		db := mysqltest.SetupDB(t)
		tuners := NewTuners(db)
		return contracttest.TunerFixture{
			Sync:     tuners,
			Reader:   tuners,
			Capacity: tuners,
			SeedTuner: func(t *testing.T, site string, tuner store.TunerInput, observedAt time.Time) {
				t.Helper()
				seedTuner(t, db, site, tuner, observedAt)
			},
			SeedDemand: func(t *testing.T, site string, programID int64, ruleID *int64, channelType, channel string,
				startAt time.Time, durationMs int64) {
				t.Helper()
				seedCapacityDemand(t, db, site, programID, ruleID, channelType, channel, startAt, durationMs)
			},
			SeedRule: func(t *testing.T, enabled, dedupeEnabled bool) int64 {
				t.Helper()
				return seedCapacityRule(t, db, enabled, dedupeEnabled)
			},
			SeedPreviewProgram: func(t *testing.T, ref store.CapacityProgramRef, channelType, channel string,
				startAt time.Time, durationMs int64) {
				t.Helper()
				seedCapacityPreviewProgram(t, db, ref, channelType, channel, startAt, durationMs)
			},
		}
	})
}

func seedTuner(t *testing.T, db *sql.DB, site string, tuner store.TunerInput, observedAt time.Time) {
	t.Helper()
	types, err := json.Marshal(tuner.Types)
	if err != nil {
		t.Fatalf("encoding tuner types: %v", err)
	}
	execFixture(t, db, `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault, observed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		site, tuner.Index, tuner.Name, string(types), tuner.IsAvailable, tuner.IsFault, observedAt)
}

func seedCapacityDemand(t *testing.T, db *sql.DB, site string, programID int64, ruleID *int64,
	channelType, channel string, startAt time.Time, durationMs int64) {
	t.Helper()
	execFixture(t, db, `
INSERT INTO program_snapshots (site, program_id, network_id, service_id, event_id, start_at, duration_ms,
                               service_name, channel_type, channel)
VALUES (?, ?, ?, ?, ?, ?, ?, 'test service', ?, ?)`,
		site, programID, fixtureNetworkID, fixtureServiceID, programID%100_000, startAt, durationMs, channelType, channel)
	execFixture(t, db, `INSERT INTO reservations (site, program_id, rule_id) VALUES (?, ?, ?)`, site, programID, ruleID)
}

func seedCapacityRule(t *testing.T, db *sql.DB, enabled, dedupeEnabled bool) int64 {
	t.Helper()
	return fixtureID(t, execFixture(t, db, `
INSERT INTO rules (name, enabled, dedupe_enabled, dedupe_threshold)
VALUES ('store capacity preview', ?, ?, CASE WHEN ? THEN 0.7 ELSE NULL END)`,
		enabled, dedupeEnabled, dedupeEnabled))
}

func seedCapacityPreviewProgram(t *testing.T, db *sql.DB, ref store.CapacityProgramRef,
	channelType, channel string, startAt time.Time, durationMs int64) {
	t.Helper()
	eventID := ref.ProgramID % 100_000
	execFixture(t, db, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel)
VALUES (?, ?, ?, 1, 0, 1, 'store preview service', ?, ?)
ON DUPLICATE KEY UPDATE observed_at = observed_at`,
		ref.Site, fixtureNetworkID, fixtureServiceID, channelType, channel)
	execFixture(t, db, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, name)
VALUES (?, ?, ?, ?, ?, ?, ?, TIMESTAMPADD(MICROSECOND, ? * 1000, ?), 'store preview program')`,
		ref.Site, ref.ProgramID, fixtureNetworkID, fixtureServiceID, eventID, startAt, durationMs, durationMs, startAt)
}
