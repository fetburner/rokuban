package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestMediaAssetCopiesMigrationBackfillsCopiesAndGenerations(t *testing.T) {
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
		t.Fatalf("clearing prior media fixtures: %v", err)
	}
	pool.Close()
	migrateDownTo(t, dbURL, 36)
	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Errorf("opening test database for cleanup: %v", err)
			return
		}
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing migration fixtures: %v", err)
		}
		cleanupPool.Close()
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()

	type legacyAsset struct {
		id             int64
		kind           string
		relPath        string
		state          string
		wantGeneration int64
		generationNull bool
	}
	cutProfile := "cut"
	dottedProfile := "x.g2.hd"
	uncutProfile := "h264"
	fixtures := []struct {
		kind, path, state string
		profile           *string
		wantGeneration    int64
		generationNull    bool
	}{
		{kind: "original", path: "sites/default/original.m2ts", state: "active", generationNull: true},
		{kind: "encoded", profile: &cutProfile, path: "sites/default/program_cut.g7.mp4", state: "active", wantGeneration: 7},
		{kind: "encoded", profile: &dottedProfile, path: "sites/default/program_x.g2.hd.g1.mp4", state: "active", wantGeneration: 1},
		{kind: "encoded", profile: &uncutProfile, path: "sites/default/program_h264.u9.mp4", state: "active", wantGeneration: 9},
		{kind: "encoded", profile: &cutProfile, path: "sites/default/program_cut.g99999999999999999999.mp4", state: "active", wantGeneration: 0},
		{kind: "thumbnail", path: "thumbnails/7.g12.jpg", state: "deleted", wantGeneration: 12},
		{kind: "seek_tiles", path: "thumbnails/7_tiles.jpg", state: "active", wantGeneration: 0},
	}
	assets := make([]legacyAsset, 0, len(fixtures))
	for i, fixture := range fixtures {
		recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
			p.EventID = int32(51000 + i)
		})
		var id int64
		var deletedAt *time.Time
		if fixture.state == "deleted" {
			at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			deletedAt = &at
		}
		if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state, deleted_at)
VALUES ($1, $2, $3, $4, 10, $5, $6)
RETURNING id`, recordingID, fixture.kind, fixture.profile, fixture.path, fixture.state, deletedAt).Scan(&id); err != nil {
			t.Fatalf("inserting legacy %s asset: %v", fixture.kind, err)
		}
		assets = append(assets, legacyAsset{
			id: id, kind: fixture.kind, relPath: fixture.path, state: fixture.state,
			wantGeneration: fixture.wantGeneration, generationNull: fixture.generationNull,
		})
	}

	migrateUpTo(t, dbURL, 39)
	for _, asset := range assets {
		assertMigratedAssetGeneration(t, pool, asset.id, asset.relPath, asset.wantGeneration, asset.generationNull)

		// 00037 leaves logical assets unassigned; the preceding release's storage adopt
		// creates their physical copy rows before 00043 drops media_assets.rel_path.
		var copies int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, asset.id).Scan(&copies); err != nil {
			t.Fatalf("counting pre-adoption copies for %s: %v", asset.kind, err)
		}
		if copies != 0 {
			t.Errorf("%s has %d copy row(s) before adoption, want none", asset.kind, copies)
		}
	}

	// The unchanged observation tables retain their legacy conflict targets.
	// missing_media_assets no longer does: issue #1652 keys it by physical copy.
	legacyOrphanPath := fmt.Sprintf("legacy-writer-%d.m2ts", assets[0].id)
	for range 2 {
		if _, err := pool.Exec(ctx, `INSERT INTO orphan_files (rel_path) VALUES ($1) ON CONFLICT (rel_path) DO NOTHING`, legacyOrphanPath); err != nil {
			t.Fatalf("legacy orphan upsert with old conflict target: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes, observed_at)
VALUES ('media', '/legacy', 100, 25, 75, now())
ON CONFLICT (root) DO UPDATE SET path = EXCLUDED.path`); err != nil {
			t.Fatalf("legacy storage sync upsert with old conflict target: %v", err)
		}
	}
	var legacyOrphanCount, legacyStorageCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orphan_files WHERE rel_path = $1`, legacyOrphanPath).Scan(&legacyOrphanCount); err != nil {
		t.Fatalf("counting old-key orphan row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM storage_sync WHERE root = 'media'`).Scan(&legacyStorageCount); err != nil {
		t.Fatalf("counting old-key storage row: %v", err)
	}
	if legacyOrphanCount != 1 || legacyStorageCount != 1 {
		t.Fatalf("legacy conflict target rows = orphan %d / storage %d, want 1 / 1", legacyOrphanCount, legacyStorageCount)
	}

	if err := adoptLegacyMediaAssetsForTest(ctx, pool); err != nil {
		t.Fatalf("adopting legacy media asset copies before dropping their source columns: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying the migration that drops media_assets.rel_path / state / deleted_at: %v", err)
	}
	assertMediaAssetCopyColumnsRetired(t, pool)

	for _, asset := range assets {
		var relPath, state string
		if err := pool.QueryRow(ctx, `
SELECT rel_path, state
FROM media_asset_copies
WHERE media_asset_id = $1 AND store = 'migration-test'`, asset.id).Scan(&relPath, &state); err != nil {
			t.Fatalf("reading adopted %s copy after migration: %v", asset.kind, err)
		}
		if relPath != asset.relPath || state != asset.state {
			t.Errorf("%s copy = (%q, %q), want (%q, %q)", asset.kind, relPath, state, asset.relPath, asset.state)
		}
	}
}

func assertMediaAssetCopyColumnsRetired(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, column := range []string{"rel_path", "state", "deleted_at"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public' AND table_name = 'media_assets' AND column_name = $1
)`, column).Scan(&exists); err != nil {
			t.Fatalf("checking media_assets.%s: %v", column, err)
		}
		if exists {
			t.Errorf("media_assets.%s should not exist after dropping them", column)
		}
	}
	for _, index := range []string{"media_assets_kind_state_idx", "media_assets_rel_path_idx"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, index).Scan(&exists); err != nil {
			t.Fatalf("checking %s: %v", index, err)
		}
		if exists {
			t.Errorf("%s should not exist after dropping them", index)
		}
	}
}

func TestDropMediaAssetCopyColumnsRefusesUnadoptedAssets(t *testing.T) {
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
		t.Fatalf("clearing prior media fixtures: %v", err)
	}
	pool.Close()
	migrateDownTo(t, dbURL, 42)
	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Errorf("opening test database for cleanup: %v", err)
			return
		}
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing migration fixtures: %v", err)
		}
		cleanupPool.Close()
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()
	recordingID := dbtest.Recording(t, pool)
	var assetID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'original', 'sites/default/unadopted.m2ts', 1)
RETURNING id`, recordingID).Scan(&assetID); err != nil {
		t.Fatalf("inserting unadopted legacy asset: %v", err)
	}

	if err := MigrateUp(ctx, dbURL); err == nil {
		t.Fatal("migration succeeded with a logical asset that had no copy")
	}
	var stillHasLegacyPath bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public' AND table_name = 'media_assets' AND column_name = 'rel_path'
)`).Scan(&stillHasLegacyPath); err != nil {
		t.Fatalf("checking rollback after blocked migration: %v", err)
	}
	if !stillHasLegacyPath {
		t.Fatal("blocked migration removed rel_path before adoption completed")
	}

	if err := adoptLegacyMediaAssetsWithGenerationsForTest(ctx, pool); err != nil {
		t.Fatalf("adopting fixture before retrying migration: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migration failed after adoption: %v", err)
	}
	assertMediaAssetCopyColumnsRetired(t, pool)
}

// 1 つの論理資産に置き場違い・世代違いのコピーが並ぶ状態は正常で、旧列には現行世代のコピー 1 つを写す。
func TestDropMediaAssetCopyColumnsDownRestoresRepresentativeCopy(t *testing.T) {
	ctx, dbURL, pool := prepareMediaAssetCopyColumnDropAtVersion42(t)
	defer pool.Close()

	deletedAt := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	type copyRow struct {
		store, path string
		generation  int64
		state       string
		deletedAt   *time.Time
	}
	fixtures := []struct {
		name            string
		kind            string
		profile         *string
		derivedGen      *int64
		copies          []copyRow
		wantPath        string
		wantState       string
		wantDeletedAtOK bool
	}{
		{
			name: "active original", kind: "original",
			copies:   []copyRow{{"a", "sites/default/orig.m2ts", 0, "active", nil}},
			wantPath: "sites/default/orig.m2ts", wantState: "active",
		},
		{
			name: "deleting original", kind: "original",
			copies:   []copyRow{{"a", "sites/default/deleting.m2ts", 0, "deleting", nil}},
			wantPath: "sites/default/deleting.m2ts", wantState: "deleting",
		},
		{
			name: "deleted original", kind: "original",
			copies:   []copyRow{{"a", "sites/default/gone.m2ts", 0, "deleted", &deletedAt}},
			wantPath: "sites/default/gone.m2ts", wantState: "deleted", wantDeletedAtOK: true,
		},
		{
			// 旧世代のコピーと tombstone は旧列に写らない。現行世代の active が代表になる。
			name: "current generation wins over older generations", kind: "encoded",
			profile: ptr("h264"), derivedGen: ptr(int64(2)),
			copies: []copyRow{
				{"a", "sites/default/p.g1.mp4", 1, "active", nil},
				{"a", "sites/default/p.g0.mp4", 0, "deleted", &deletedAt},
				{"a", "sites/default/p.g2.mp4", 2, "active", nil},
			},
			wantPath: "sites/default/p.g2.mp4", wantState: "active",
		},
		{
			// 置き場違いの現行コピーは active を先に選び、state は現行世代全体で決める。
			name: "active copy in another store is the representative", kind: "thumbnail",
			derivedGen: ptr(int64(0)),
			copies: []copyRow{
				{"a", "thumbnails/1.jpg", 0, "deleting", nil},
				{"b", "thumbnails/1.b.jpg", 0, "active", nil},
			},
			wantPath: "thumbnails/1.b.jpg", wantState: "active",
		},
	}
	assets := make([]int64, len(fixtures))
	for i, fixture := range fixtures {
		recordingID := dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
			params.EventID = int32(52000 + i)
		})
		if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, derived_generation)
VALUES ($1, $2, $3, $5, 10, $4)
RETURNING id`, recordingID, fixture.kind, fixture.profile, fixture.derivedGen, fmt.Sprintf("legacy-mirror-%d", i)).Scan(&assets[i]); err != nil {
			t.Fatalf("%s: inserting asset: %v", fixture.name, err)
		}
		for _, c := range fixture.copies {
			if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, deleted_at)
VALUES ($1, $2, $3, $4, $5, $6)`, assets[i], c.store, c.path, c.generation, c.state, c.deletedAt); err != nil {
				t.Fatalf("%s: inserting copy %s: %v", fixture.name, c.path, err)
			}
		}
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying the column drop: %v", err)
	}
	assertMediaAssetCopyColumnsRetired(t, pool)

	migrateDownTo(t, dbURL, 42)
	for i, fixture := range fixtures {
		var gotPath, gotState string
		var gotDeletedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT rel_path, state, deleted_at FROM media_assets WHERE id = $1`, assets[i]).Scan(&gotPath, &gotState, &gotDeletedAt); err != nil {
			t.Fatalf("%s: reading restored asset: %v", fixture.name, err)
		}
		if gotPath != fixture.wantPath || gotState != fixture.wantState {
			t.Errorf("%s: restored (%q, %q), want (%q, %q)", fixture.name, gotPath, gotState, fixture.wantPath, fixture.wantState)
		}
		if fixture.wantDeletedAtOK {
			if gotDeletedAt == nil || !gotDeletedAt.Equal(deletedAt) {
				t.Errorf("%s: restored deleted_at = %v, want %s", fixture.name, gotDeletedAt, deletedAt)
			}
		} else if gotDeletedAt != nil {
			t.Errorf("%s: restored deleted_at = %s, want NULL", fixture.name, gotDeletedAt)
		}
	}
	assertDroppedMediaAssetCopyColumnsRestored(t, pool)

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("reapplying the column drop after rollback: %v", err)
	}
	assertMediaAssetCopyColumnsRetired(t, pool)
}

func TestDropMediaAssetCopyColumnsDownRefusesUnrepresentableState(t *testing.T) {
	tests := []struct {
		name       string
		assetCount int
		wantError  string
		mutate     func(context.Context, *pgxpool.Pool, []int64) error
	}{
		{
			name:       "no copy",
			assetCount: 1,
			wantError:  "no copy of its current generation",
			mutate: func(ctx context.Context, pool *pgxpool.Pool, assetIDs []int64) error {
				_, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetIDs[0])
				return err
			},
		},
		{
			name:       "only an older generation remains",
			assetCount: 1,
			wantError:  "no copy of its current generation",
			mutate: func(ctx context.Context, pool *pgxpool.Pool, assetIDs []int64) error {
				// 派生物ではないので、世代カウンターを進めて現行世代のコピーを無くす。
				_, err := pool.Exec(ctx, `
UPDATE media_assets SET kind = 'encoded', profile = 'h264', derived_generation = 3 WHERE id = $1`, assetIDs[0])
				return err
			},
		},
		{
			name:       "pending copy",
			assetCount: 1,
			wantError:  "while a copy is pending",
			mutate: func(ctx context.Context, pool *pgxpool.Pool, assetIDs []int64) error {
				_, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'pending' WHERE media_asset_id = $1`, assetIDs[0])
				return err
			},
		},
		{
			name:       "same live path in different stores",
			assetCount: 2,
			wantError:  "while live assets share a path",
			mutate: func(ctx context.Context, pool *pgxpool.Pool, assetIDs []int64) error {
				_, err := pool.Exec(ctx, `
UPDATE media_asset_copies AS target
SET store = 'secondary', rel_path = source.rel_path
FROM media_asset_copies AS source
WHERE target.media_asset_id = $1 AND source.media_asset_id = $2`, assetIDs[1], assetIDs[0])
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, dbURL, pool := prepareMediaAssetCopyColumnDropAtVersion42(t)
			defer pool.Close()

			assetIDs := make([]int64, 0, tt.assetCount)
			for i := range tt.assetCount {
				recordingID := dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
					params.EventID = int32(52100 + i)
				})
				var assetID int64
				if err := pool.QueryRow(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'original', $2, 10)
RETURNING id`, recordingID, fmt.Sprintf("sites/default/lossy-%d.m2ts", i)).Scan(&assetID); err != nil {
					t.Fatalf("inserting original asset: %v", err)
				}
				assetIDs = append(assetIDs, assetID)
			}
			if err := adoptLegacyMediaAssetsWithGenerationsForTest(ctx, pool); err != nil {
				t.Fatalf("creating copies: %v", err)
			}
			if err := MigrateUp(ctx, dbURL); err != nil {
				t.Fatalf("applying the column drop: %v", err)
			}
			if err := tt.mutate(ctx, pool, assetIDs); err != nil {
				t.Fatalf("creating the unrepresentable state: %v", err)
			}

			err := runGooseMigration(ctx, dbURL, func(ctx context.Context, provider *goose.Provider) error {
				_, err := provider.DownTo(ctx, 42)
				return err
			})
			if err == nil {
				t.Fatal("migration down succeeded for a state the dropped columns cannot represent")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("migration down error = %q, want it to contain %q", err, tt.wantError)
			}
			assertMediaAssetCopyColumnsRetired(t, pool)
			assertMediaAssetCopyColumnsStillDropped(t, pool)

			var version int64
			if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, provider *goose.Provider) error {
				var err error
				version, err = provider.GetDBVersion(ctx)
				return err
			}); err != nil {
				t.Fatalf("reading applied migration version after refused rollback: %v", err)
			}
			if version != 43 {
				t.Fatalf("applied migration version after refused rollback = %d, want 43", version)
			}
		})
	}
}

func prepareMediaAssetCopyColumnDropAtVersion42(t *testing.T) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
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
		t.Fatalf("clearing prior migration fixtures: %v", err)
	}
	pool.Close()
	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Errorf("opening test database for cleanup: %v", err)
			return
		}
		if _, err := cleanupPool.Exec(context.Background(), `TRUNCATE recordings CASCADE`); err != nil {
			t.Errorf("clearing migration fixtures: %v", err)
		}
		cleanupPool.Close()
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	migrateDownTo(t, dbURL, 42)
	pool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database at migration 42: %v", err)
	}
	return ctx, dbURL, pool
}

func assertDroppedMediaAssetCopyColumnsRestored(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, view := range []string{"until_encoded_deletable_originals", "removed_encoded_assets"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.views
    WHERE table_schema = 'public' AND table_name = $1
)`, view).Scan(&exists); err != nil {
			t.Fatalf("checking restored view %s: %v", view, err)
		}
		if !exists {
			t.Errorf("legacy view %s was not restored", view)
		}
	}
	for _, index := range []string{"media_assets_kind_state_idx", "media_assets_rel_path_idx"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_indexes
    WHERE schemaname = 'public' AND indexname = $1
)`, index).Scan(&exists); err != nil {
			t.Fatalf("checking restored index %s: %v", index, err)
		}
		if !exists {
			t.Errorf("legacy index %s was not restored", index)
		}
	}
}

func assertMediaAssetCopyColumnsStillDropped(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM information_schema.views
    WHERE table_schema = 'public' AND table_name = 'removed_encoded_assets'
)`).Scan(&exists); err != nil {
		t.Fatalf("checking the current removed_encoded_assets view after refused rollback: %v", err)
	}
	if !exists {
		t.Fatal("refused rollback removed the current removed_encoded_assets view")
	}
}

func assertMigratedAssetGeneration(t *testing.T, pool *pgxpool.Pool, assetID int64, relPath string, want int64, wantNull bool) {
	t.Helper()
	var got *int64
	if err := pool.QueryRow(context.Background(), `SELECT derived_generation FROM media_assets WHERE id = $1`, assetID).Scan(&got); err != nil {
		t.Fatalf("reading derived generation for %s: %v", relPath, err)
	}
	if wantNull {
		if got != nil {
			t.Errorf("%s derived_generation = %d, want NULL", relPath, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s derived_generation is NULL, want %d", relPath, want)
	} else if *got != want {
		t.Errorf("%s derived_generation = %d, want %d", relPath, *got, want)
	}
}
