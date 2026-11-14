package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// mediaAssetsIndexesLeadingRecordingID は、media_assets の索引のうち先頭列が
// recording_id のものを名前順で返す。
func mediaAssetsIndexesLeadingRecordingID(t *testing.T, dbURL string) []string {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `
SELECT c.relname
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = 'public.media_assets'::regclass
  AND pg_get_indexdef(i.indexrelid, 1, true) = 'recording_id'
ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("listing media_assets indexes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning index name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing media_assets indexes: %v", err)
	}
	return names
}

// 単列の recording_id 索引を落としても、recording_id で引く読み手は一意制約の
// 索引の先頭列で引ける。一意制約の列順が変わればこの前提が崩れるので、それも検査する。
func TestDropMediaAssetsRecordingIDIndexMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	got := mediaAssetsIndexesLeadingRecordingID(t, dbURL)
	if len(got) != 1 || got[0] != "media_assets_recording_id_kind_profile_key" {
		t.Fatalf("indexes leading with recording_id at latest = %v, want [media_assets_recording_id_kind_profile_key]", got)
	}

	migrateDownTo(t, dbURL, 60)
	got = mediaAssetsIndexesLeadingRecordingID(t, dbURL)
	if len(got) != 2 || got[0] != "media_assets_recording_id_idx" || got[1] != "media_assets_recording_id_kind_profile_key" {
		t.Fatalf("indexes leading with recording_id before migration 61 = %v, want [media_assets_recording_id_idx media_assets_recording_id_kind_profile_key]", got)
	}
}
