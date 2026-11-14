package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestMediaAssetDerivedGenerationMigrationBackfillsAndRejectsNull(t *testing.T) {
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
	migrateDownTo(t, dbURL, 57)
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
		t.Fatalf("opening version 57 test database: %v", err)
	}
	recordingID := dbtest.Recording(t, pool)
	for _, asset := range []struct {
		kind    string
		profile *string
	}{
		{kind: "thumbnail"},
		{kind: "seek_tiles"},
		{kind: "encoded", profile: stringPointer("h264")},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES ($1, $2, $3, 1, NULL)`, recordingID, asset.kind, asset.profile); err != nil {
			pool.Close()
			t.Fatalf("seeding NULL %s asset: %v", asset.kind, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'original', 1, NULL)`, recordingID); err != nil {
		pool.Close()
		t.Fatalf("seeding NULL original generation: %v", err)
	}
	pool.Close()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying derived-generation migration: %v", err)
	}
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening migrated test database: %v", err)
	}
	defer pool.Close()

	var derivedCount, zeroCount, nullCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind <> 'original'),
       count(*) FILTER (WHERE kind <> 'original' AND derived_generation = 0),
       count(*) FILTER (WHERE derived_generation IS NULL)
FROM media_assets WHERE recording_id = $1`, recordingID).Scan(&derivedCount, &zeroCount, &nullCount); err != nil {
		t.Fatalf("reading backfilled generations: %v", err)
	}
	if derivedCount != 3 || zeroCount != derivedCount || nullCount != 0 {
		t.Fatalf("backfilled generations = derived %d, zero %d, null %d; want 3, 3, 0", derivedCount, zeroCount, nullCount)
	}

	var defaultGeneration int64
	newRecordingID := dbtest.Recording(t, pool)
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes)
VALUES ($1, 'thumbnail', 1)
RETURNING derived_generation`, newRecordingID).Scan(&defaultGeneration); err != nil {
		t.Fatalf("inserting without derived_generation as an older binary does: %v", err)
	}
	if defaultGeneration != 0 {
		t.Fatalf("omitted derived_generation default = %d, want 0", defaultGeneration)
	}

	nullRecordingID := dbtest.Recording(t, pool)
	_, err = pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
	VALUES ($1, 'thumbnail', 1, NULL)`, nullRecordingID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Fatalf("inserting NULL for a derived asset error = %v, want not-null violation", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'original', 1, 1)`, newRecordingID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("inserting original generation 1 error = %v, want CHECK violation", err)
	}
	pool.Close()

	migrateDownTo(t, dbURL, 57)

	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening downgraded test database: %v", err)
	}
	defer pool.Close()
	var originalCount, nullOriginalCount, zeroDerivedCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'original'),
       count(*) FILTER (WHERE kind = 'original' AND derived_generation IS NULL),
       count(*) FILTER (WHERE kind <> 'original' AND derived_generation = 0)
FROM media_assets WHERE recording_id = $1`, recordingID).Scan(
		&originalCount, &nullOriginalCount, &zeroDerivedCount,
	); err != nil {
		t.Fatalf("reading downgraded generations: %v", err)
	}
	if originalCount != 1 || nullOriginalCount != 1 || zeroDerivedCount != 3 {
		t.Fatalf("downgraded generations = originals %d, NULL originals %d, zero derived %d; want 1, 1, 3",
			originalCount, nullOriginalCount, zeroDerivedCount)
	}
}

func stringPointer(value string) *string { return &value }
