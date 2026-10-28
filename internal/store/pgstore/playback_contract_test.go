package pgstore

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var playbackContractEventID atomic.Int32

func TestPlaybackStateContract(t *testing.T) {
	contracttest.RunPlayback(t, func(t *testing.T) contracttest.PlaybackFixture {
		pool := testutil.SetupDB(t)
		return contracttest.PlaybackFixture{
			Store: NewPlayback(pool), Recordings: NewRecordings(pool, dbtest.StoreName),
			NewRecording: func(t *testing.T, options contracttest.RecordingOptions) int64 {
				return newContractRecording(t, pool, options, &playbackContractEventID)
			},
			FailPositionDelete: failContractPositionDelete(pool),
		}
	})
}

func failContractPositionDelete(pool *pgxpool.Pool) func(t *testing.T) func() {
	return func(t *testing.T) func() {
		t.Helper()
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `
CREATE FUNCTION playback_contract_reject_position_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION 'injected playback position delete failure';
END;
$$`); err != nil {
			t.Fatalf("creating position delete failure function: %v", err)
		}
		if _, err := pool.Exec(ctx, `
CREATE TRIGGER playback_contract_reject_position_delete
BEFORE DELETE ON recording_playback_positions
FOR EACH ROW EXECUTE FUNCTION playback_contract_reject_position_delete()`); err != nil {
			_, _ = pool.Exec(ctx, `DROP FUNCTION playback_contract_reject_position_delete()`)
			t.Fatalf("creating position delete failure trigger: %v", err)
		}
		return func() {
			if _, err := pool.Exec(context.Background(), `DROP TRIGGER playback_contract_reject_position_delete ON recording_playback_positions`); err != nil {
				t.Errorf("dropping position delete failure trigger: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `DROP FUNCTION playback_contract_reject_position_delete()`); err != nil {
				t.Errorf("dropping position delete failure function: %v", err)
			}
		}
	}
}

func newContractRecording(t *testing.T, pool *pgxpool.Pool, options contracttest.RecordingOptions, eventIDs *atomic.Int32) int64 {
	t.Helper()
	start := options.ProgramStartAt
	if start.IsZero() {
		start = time.Now().UTC().Truncate(time.Second)
	}
	eventID := options.EventID
	if eventID == 0 {
		eventID = eventIDs.Add(1)
	}
	id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.EventID = eventID
		fixtureParams.Title = "store contract fixture"
		fixtureParams.ProgramStartAt = start
	})

	if options.CMDetectionEnabled {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', true)`, id); err != nil {
			t.Fatalf("enabling CM detection: %v", err)
		}
	}
	if options.Trashed || options.Purged {
		if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("trashing recording: %v", err)
		}
	}
	if options.Purged {
		if _, err := pool.Exec(context.Background(), `UPDATE recordings SET purged_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("purging recording: %v", err)
		}
	}
	return id
}
