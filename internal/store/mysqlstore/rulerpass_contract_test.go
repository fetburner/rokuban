package mysqlstore

import (
	"database/sql"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/serieskey"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestRulerPassContract(t *testing.T) {
	t.Parallel()
	contracttest.RunRulerPass(t, func(t *testing.T) contracttest.RulerPassFixture {
		db := mysqltest.SetupDB(t)
		reservations := NewReservations(db)
		return contracttest.RulerPassFixture{
			Pass:         reservations,
			Bumper:       reservations,
			Reservations: reservations,
			Breakers:     NewBreakers(db),
			NewProgram: func(t *testing.T, title string) contracttest.RulerProgram {
				return seedReservationProgram(t, db, title, contracttest.ReservationProgramOptions{Projection: true, Snapshot: true})
			},
			NewRule: func(t *testing.T, name string, dedupeEnabled bool, threshold float32) int64 {
				var value *float32
				if dedupeEnabled {
					value = &threshold
				}
				return fixtureID(t, execFixture(t, db, `
    INSERT INTO rules (name, dedupe_enabled, dedupe_threshold) VALUES (?, ?, ?)
    `, name, dedupeEnabled, value))
			},
			NewFinishedRecording: func(t *testing.T, ruleID int64, program contracttest.RulerProgram, title string) int64 {
				return seedFinishedRecording(t, db, ruleID, program, title)
			},
			SetRuleOrder: func(t *testing.T, ruleID int64, enabled bool, priority int32) {
				execFixture(t, db, `UPDATE rules SET enabled = ?, priority = ? WHERE id = ?`, enabled, priority, ruleID)
			},
			CommitOriginal: func(t *testing.T, recordingID int64) {
				execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 1)`, recordingID)
			},
			SetProgramStarts: func(t *testing.T, p contracttest.RulerProgram, snapshotStart, projectionStart time.Time) {
				execFixture(t, db, `UPDATE program_snapshots SET start_at = ? WHERE site = ? AND program_id = ?`,
					snapshotStart, p.Site, p.ProgramID)
				execFixture(t, db, `UPDATE epg_programs SET start_at = ?, end_at = ? WHERE site = ? AND program_id = ?`,
					projectionStart, projectionStart.Add(30*time.Minute), p.Site, p.ProgramID)
			},
			SetDedupeWindow: func(t *testing.T, ruleID int64, window time.Duration) {
				execFixture(t, db, `UPDATE rules SET dedupe_window = ? WHERE id = ?`, int64(window/time.Second), ruleID)
			},
			SetRecordingStartAt: func(t *testing.T, recordingID int64, start time.Time) {
				execFixture(t, db, `UPDATE recordings SET program_start_at = ? WHERE id = ?`, start, recordingID)
			},
			FailBreakerTrip: func(t *testing.T, name string) {
				// 各ケースに専用 DB があり、ブレーカーを書くのはこのパスだけである。
				execFixture(t, db, `
    CREATE TRIGGER reject_ruler_breaker BEFORE INSERT ON circuit_breakers FOR EACH ROW
      SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'ruler pass breaker write rejected'
    `)
			},
		}
	})
}

func seedFinishedRecording(t *testing.T, db *sql.DB, ruleID int64, p contracttest.RulerProgram, title string) int64 {
	t.Helper()
	var series *string
	if key, ok := serieskey.SeriesKey(title); ok {
		series = &key
	}
	return fixtureID(t, execFixture(t, db, `
 INSERT INTO recordings (rule_id, source, site, network_id, service_id, event_id, service_name, channel_type,
                         channel, title, program_start_at, program_duration_ms, status, series_key)
 VALUES (?, 'rule', ?, ?, ?, ?, 'store contract', 'GR', '27', ?, ?, 1800000, 'finished', ?)
 `, ruleID, p.Site, fixtureNetworkID, fixtureServiceID, p.EventID, title, p.StartAt.Add(-48*time.Hour), series))
}
