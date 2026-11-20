package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestEncodeAttemptState_ReadsTheRowOrReportsAbsent(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	if _, found := dbtest.EncodeAttemptState(t, pool, recordingID, "h264"); found {
		t.Fatal("attempt found before any row exists")
	}
	testutil.MustExec(t, pool, "inserting a failed attempt", `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'h264', 'failed', now(), 1)`, recordingID)
	if state, found := dbtest.EncodeAttemptState(t, pool, recordingID, "h264"); !found || state != "failed" {
		t.Fatalf("attempt state = %q (found %v), want failed", state, found)
	}
	if _, found := dbtest.EncodeAttemptState(t, pool, recordingID, "cut"); found {
		t.Fatal("attempt for another profile reported as present")
	}
}
