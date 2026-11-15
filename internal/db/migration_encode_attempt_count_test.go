package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestEncodeAttemptCountMigrationBackfillsZero(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database before setup: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE recordings CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("clearing prior fixtures: %v", err)
	}
	pool.Close()
	migrateDownTo(t, dbURL, 63)
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
			return
		}
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Errorf("opening test database for cleanup: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing migration fixtures: %v", err)
		}
	})

	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening version 63 test database: %v", err)
	}

	recordingID := dbtest.Recording(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempt_count)
VALUES ($1, 'h264-failed', 'failed', 0), ($1, 'h264-running', 'running', 0)`, recordingID); err != nil {
		pool.Close()
		t.Fatalf("seeding zero-count encode attempts: %v", err)
	}
	pool.Close()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying encode attempt-count migration: %v", err)
	}
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening migrated test database: %v", err)
	}
	defer pool.Close()

	for _, fixture := range []struct {
		profile   string
		exhausted bool
	}{
		{profile: "h264-failed", exhausted: true},
		{profile: "h264-running", exhausted: false},
	} {
		var count int32
		var exhausted bool
		if err := pool.QueryRow(ctx, `
SELECT attempt_count, encode_attempts_exhausted(recording_id, profile, 1)
FROM recording_encode_attempts
WHERE recording_id = $1 AND profile = $2`, recordingID, fixture.profile).Scan(&count, &exhausted); err != nil {
			t.Fatalf("reading migrated %s attempt: %v", fixture.profile, err)
		}
		if count != 1 || exhausted != fixture.exhausted {
			t.Fatalf("%s: count = %d, exhausted = %t; want 1, %t", fixture.profile, count, exhausted, fixture.exhausted)
		}
	}
}
