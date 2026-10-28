package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

// migrateDownTo は version まで戻す。最新から数えて Down を繰り返す形にしないのは、
// migration を足すたびに戻り先がずれ、別の版を検査するテストになるからである。
func migrateDownTo(t *testing.T, dbURL string, version int64) {
	t.Helper()
	if err := runGooseMigration(context.Background(), dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.DownTo(ctx, version)
		return err
	}); err != nil {
		t.Fatalf("migrating down to version %d: %v", version, err)
	}
}

func migrateUpTo(t *testing.T, dbURL string, version int64) {
	t.Helper()
	if err := runGooseMigration(context.Background(), dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, version)
		return err
	}); err != nil {
		t.Fatalf("migrating up to version %d: %v", version, err)
	}
}

func TestTSScanMigrationBackfillsOnlyOriginalsWithDropStats(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	migrateDownTo(t, dbURL, 23) // TS 計測記録の表（00024）より前
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
		return dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
			params.NetworkID = 32736
			params.ServiceID = 1024
			params.EventID = eventID
			params.ServiceName = "migration test"
			params.Title = "migration test"
			params.ProgramDurationMs = 60_000
		})
	}
	createAsset := func(recordingID int64, kind string, profile *string, size int64) int64 {
		t.Helper()
		switch kind {
		case "original":
			return createPre24MediaAsset(t, pool, recordingID, kind, nil, fmt.Sprintf("migration-test/%d", recordingID), size)
		case "encoded":
			if profile == nil {
				t.Fatal("encoded migration fixture requires a profile")
			}
			return createPre24MediaAsset(t, pool, recordingID, kind, profile, fmt.Sprintf("migration-test/%d", recordingID), size)
		default:
			t.Fatalf("unsupported media asset kind %q", kind)
			return 0
		}
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

	migrateUpTo(t, dbURL, 39)
	if err := adoptLegacyMediaAssetsForTest(ctx, pool); err != nil {
		t.Fatalf("adopting media asset copies before compatibility-column removal: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying TS scan and media asset copy migrations: %v", err)
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
	migrateDownTo(t, dbURL, 24) // until_encoded が計測を要求する移行（00025）より前
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
		return dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
			params.NetworkID = 32736
			params.ServiceID = 1024
			params.EventID = eventID
			params.ServiceName = "migration test"
			params.Title = "migration test"
			params.ProgramDurationMs = 60_000
		})
	}
	createOriginal := func(recordingID int64, size int64) int64 {
		t.Helper()
		return createPre24MediaAsset(t, pool, recordingID, "original", nil, fmt.Sprintf("retention-migration-test/%d", recordingID), size)
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
	encodedID := createPre24MediaAsset(t, pool, createRecording(23104), "encoded", &profile, "retention-migration-test/encoded", 21)
	addDropStat(encodedID)

	migrateUpTo(t, dbURL, 39)
	if err := adoptLegacyMediaAssetsForTest(ctx, pool); err != nil {
		t.Fatalf("adopting media asset copies before compatibility-column removal: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying original-retention and media asset copy migrations: %v", err)
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

func createPre24MediaAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string, profile *string, relPath string, sizeBytes int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`, recordingID, kind, profile, relPath, sizeBytes).Scan(&id)
	if err != nil {
		t.Fatalf("creating pre-24 media asset: %v", err)
	}
	return id
}

func adoptLegacyMediaAssetsForTest(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT id, 'migration-test', rel_path, state, created_at, deleted_at
FROM media_assets`)
	return err
}
