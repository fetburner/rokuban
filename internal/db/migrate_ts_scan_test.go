package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTSScanMigrationBackfillsOnlyOriginalsWithDropStats(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("rolling back original-retention migration: %v", err)
	}
	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("rolling back TS scan migration: %v", err)
	}
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()

	createRecording := func(eventID int32) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO recordings (
				source, site, network_id, service_id, event_id,
				service_name, channel_type, channel, title,
				program_start_at, program_duration_ms, status
			) VALUES (
				'manual', 'default', 32736, 1024, $1,
				'migration test', 'GR', '27', 'migration test',
				now(), 60000, 'finished'
			) RETURNING id`, eventID).Scan(&id)
		if err != nil {
			t.Fatalf("creating recording: %v", err)
		}
		return id
	}
	createAsset := func(recordingID int64, kind string, profile *string, size int64) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			recordingID, kind, profile, fmt.Sprintf("migration-test/%d", recordingID), size).Scan(&id)
		if err != nil {
			t.Fatalf("creating %s asset: %v", kind, err)
		}
		return id
	}
	addDropStat := func(assetID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled)
			VALUES ($1, 256, 1, 0, 0, 0)`, assetID); err != nil {
			t.Fatalf("creating drop_stats: %v", err)
		}
	}

	withStatsID := createAsset(createRecording(23001), "original", nil, 42)
	addDropStat(withStatsID)
	withoutStatsID := createAsset(createRecording(23002), "original", nil, 84)
	profile := "mobile"
	encodedID := createAsset(createRecording(23003), "encoded", &profile, 126)
	addDropStat(encodedID)

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying TS scan migration: %v", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT media_asset_id, scanned_size_bytes
		FROM media_asset_ts_scans ORDER BY media_asset_id`)
	if err != nil {
		t.Fatalf("querying backfilled scan records: %v", err)
	}
	got := map[int64]int64{}
	for rows.Next() {
		var assetID, size int64
		if err := rows.Scan(&assetID, &size); err != nil {
			rows.Close()
			t.Fatalf("scanning backfilled scan record: %v", err)
		}
		got[assetID] = size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterating backfilled scan records: %v", err)
	}
	rows.Close()
	want := map[int64]int64{withStatsID: 42}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backfilled scan records = %v, want %v (only originals with drop_stats)", got, want)
	}
	if withoutStatsID == withStatsID || encodedID == withStatsID {
		t.Fatal("test fixtures unexpectedly share an asset ID")
	}
}

func TestUntilEncodedMigrationBackfillsMissingOriginalScanMarkers(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("rolling back original-retention migration: %v", err)
	}
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()

	createRecording := func(eventID int32) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO recordings (
				source, site, network_id, service_id, event_id,
				service_name, channel_type, channel, title,
				program_start_at, program_duration_ms, status
			) VALUES (
				'manual', 'default', 32736, 1024, $1,
				'migration test', 'GR', '27', 'migration test',
				now(), 60000, 'finished'
			) RETURNING id`, eventID).Scan(&id)
		if err != nil {
			t.Fatalf("creating recording: %v", err)
		}
		return id
	}
	createOriginal := func(recordingID int64, size int64) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
			VALUES ($1, 'original', $2, $3) RETURNING id`,
			recordingID, fmt.Sprintf("retention-migration-test/%d", recordingID), size).Scan(&id)
		if err != nil {
			t.Fatalf("creating original asset: %v", err)
		}
		return id
	}
	addDropStat := func(assetID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled)
			VALUES ($1, 256, 1, 0, 0, 0)`, assetID); err != nil {
			t.Fatalf("creating drop_stats: %v", err)
		}
	}

	missingMarkerID := createOriginal(createRecording(23101), 42)
	addDropStat(missingMarkerID)
	noStatsID := createOriginal(createRecording(23102), 84)
	withExistingMarkerID := createOriginal(createRecording(23103), 126)
	addDropStat(withExistingMarkerID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
		VALUES ($1, 120)`, withExistingMarkerID); err != nil {
		t.Fatalf("creating existing scan marker: %v", err)
	}
	profile := "mobile"
	var encodedID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
		VALUES ($1, 'encoded', $2, 'retention-migration-test/encoded', 21) RETURNING id`,
		createRecording(23104), &profile).Scan(&encodedID); err != nil {
		t.Fatalf("creating encoded asset: %v", err)
	}
	addDropStat(encodedID)

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying original-retention migration: %v", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT media_asset_id, scanned_size_bytes
		FROM media_asset_ts_scans
		WHERE media_asset_id = ANY($1::bigint[])
		ORDER BY media_asset_id`, []int64{missingMarkerID, noStatsID, withExistingMarkerID, encodedID})
	if err != nil {
		t.Fatalf("querying scan markers after migration: %v", err)
	}
	got := map[int64]int64{}
	for rows.Next() {
		var assetID, size int64
		if err := rows.Scan(&assetID, &size); err != nil {
			rows.Close()
			t.Fatalf("scanning marker: %v", err)
		}
		got[assetID] = size
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterating markers: %v", err)
	}
	rows.Close()
	want := map[int64]int64{missingMarkerID: 42, withExistingMarkerID: 120}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan markers = %v, want %v (only missing originals with existing drop_stats are backfilled)", got, want)
	}
}
