package mysqlstore

import (
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestPlaybackStateContract(t *testing.T) {
	contracttest.RunPlayback(t, func(t *testing.T) contracttest.PlaybackFixture {
		db := mysqltest.SetupDB(t)
		var eventSequence atomic.Int32
		newRecording := func(t *testing.T, o contracttest.RecordingOptions) int64 {
			return newContractRecording(t, db, o, &eventSequence)
		}
		return contracttest.PlaybackFixture{
			Store:        NewPlayback(db),
			Recordings:   NewRecordingQueries(db, ""),
			NewRecording: newRecording,
			FailPositionDelete: func(t *testing.T) func() {
				t.Helper()
				execFixture(t, db, `CREATE TRIGGER playback_contract_reject_position_delete BEFORE DELETE ON recording_playback_positions FOR EACH ROW
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected playback position delete failure'`)
				return func() {
					execFixture(t, db, `DROP TRIGGER IF EXISTS playback_contract_reject_position_delete`)
				}
			},
		}
	})
}

// newContractRecording は契約テストの録画を 1 行作る。RecordingOptions の全フィールドを扱う。
// EventID が 0 なら eventIDs から採番する。pgstore の newContractRecording と同じ意味にしてある。
func newContractRecording(t *testing.T, db *sql.DB, options contracttest.RecordingOptions, eventIDs *atomic.Int32) int64 {
	t.Helper()
	eventID := options.EventID
	if eventID == 0 {
		eventID = eventIDs.Add(1)
	}
	start := options.ProgramStartAt
	if start.IsZero() {
		start = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	}
	duration := options.ProgramDurationMs
	if duration == 0 {
		duration = 1_800_000
	}
	genres, genreLv1, err := recordingQueryGenres(nil)
	if err != nil {
		t.Fatalf("encoding fixture genres: %v", err)
	}
	var deletedAt, purgedAt any
	if options.Trashed || options.Purged {
		deletedAt = time.Now().UTC()
	}
	if options.Purged {
		purgedAt = deletedAt
	}
	id := fixtureID(t, execFixture(t, db, `
INSERT INTO recordings (site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, genres, is_free, program_start_at, program_duration_ms, status, source, genre_lv1, deleted_at, purged_at)
VALUES (?, 32678, 5168, ?, 'contract service', 'GR', 'contract channel',
    'store contract fixture', ?, TRUE, ?, ?, 'finished', 'manual', ?, ?, ?)`,
		store.DefaultSite, eventID, genres, start, duration, genreLv1, deletedAt, purgedAt))
	if options.CMDetectionEnabled {
		execFixture(t, db, `INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect) VALUES (?, 'always', JSON_ARRAY(), TRUE)`, id)
	}
	return id
}
