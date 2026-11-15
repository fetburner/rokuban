package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestRecordingEncodeAttemptCountConstraint(t *testing.T) {
	pool := setupTestDB(t)
	recordingID := dbtest.Recording(t, pool)
	ctx := context.Background()
	for _, fixture := range []struct {
		profile string
		state   string
	}{
		{profile: "h264-running", state: "running"},
		{profile: "h264-failed", state: "failed"},
	} {
		_, err := pool.Exec(ctx, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempt_count)
VALUES ($1, $2, $3, 0)`, recordingID, fixture.profile, fixture.state)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "recording_encode_attempts_attempt_count_check" {
			t.Fatalf("%s count 0: want attempt_count check violation, got %v", fixture.state, err)
		}
		var count int32
		err = pool.QueryRow(ctx, `
INSERT INTO recording_encode_attempts (recording_id, profile, state)
VALUES ($1, $2, $3)
RETURNING attempt_count`, recordingID, fixture.profile, fixture.state).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("%s default count: got %d, %v; want 1", fixture.state, count, err)
		}
	}
}
