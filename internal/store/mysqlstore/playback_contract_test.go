package mysqlstore

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestPlaybackStateContract(t *testing.T) {
	contracttest.RunPlayback(t, func(t *testing.T) contracttest.PlaybackFixture {
		db := mysqltest.SetupDB(t)
		var eventSequence atomic.Int32
		newRecording := func(t *testing.T, o contracttest.RecordingOptions) int64 {
			t.Helper()
			if o.Trashed {
				t.Fatalf("playback contract fixture does not support Trashed")
			}
			if o.CMDetectionEnabled {
				t.Fatalf("playback contract fixture does not support CMDetectionEnabled")
			}
			eventID := o.EventID
			if eventID == 0 {
				eventID = eventSequence.Add(1)
			}
			start := o.ProgramStartAt
			if start.IsZero() {
				start = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			}
			genres, genreLv1, err := recordingQueryGenres(nil)
			if err != nil {
				t.Fatalf("encoding fixture genres: %v", err)
			}
			var deletedAt, purgedAt any
			if o.Purged {
				deletedAt = time.Now().UTC()
				purgedAt = deletedAt
			}
			id, err := execFixture(t, db, `
INSERT INTO recordings (site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, genres, is_free, program_start_at, program_duration_ms, status, source, genre_lv1, deleted_at, purged_at)
VALUES ('contract-site', 32678, 5168, ?, 'contract service', 'GR', 'contract channel',
    'playback contract fixture', ?, TRUE, ?, 1800000, 'finished', 'manual', ?, ?, ?)`,
				eventID, genres, start, genreLv1, deletedAt, purgedAt).LastInsertId()
			if err != nil {
				t.Fatalf("reading fixture recording ID: %v", err)
			}
			return id
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
