package db

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestTombstoneForeignKeysUseNoAction(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
SELECT conrelid::regclass::text, conname, confrelid::regclass::text, confdeltype::text
FROM pg_constraint
WHERE contype = 'f' AND confrelid IN ('recordings'::regclass, 'media_assets'::regclass)
ORDER BY conname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	parents := map[string]int{}
	for rows.Next() {
		var table, constraint, parent, action string
		if err := rows.Scan(&table, &constraint, &parent, &action); err != nil {
			t.Fatal(err)
		}
		parents[parent]++
		if action != "a" {
			t.Errorf("%s.%s references %s with delete action %q, want NO ACTION (a)", table, constraint, parent, action)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"recordings", "media_assets"} {
		if parents[parent] == 0 {
			t.Errorf("no foreign keys found for %s", parent)
		}
	}
}

func TestTombstoneDeletePreservesSatellite(t *testing.T) {
	for _, parent := range []string{"recordings", "media_assets"} {
		t.Run(parent, func(t *testing.T) {
			pool := setupTestDB(t)
			ctx := context.Background()
			id := dbtest.Recording(t, pool)
			satellite := "recording_processing_policy"
			key := "recording_id"
			if parent == "recordings" {
				if _, err := pool.Exec(ctx, `INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles) VALUES ($1, 'always', '{}')`, id); err != nil {
					t.Fatal(err)
				}
			} else {
				id = dbtest.MediaAsset(t, pool, id)
				satellite = "media_asset_copies"
				key = "media_asset_id"
			}
			_, err := pool.Exec(ctx, "DELETE FROM "+parent+" WHERE id = $1", id)
			assertPgError(t, err, "23503")
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+satellite+" WHERE "+key+" = $1", id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("satellite rows after rejected delete = %d, want 1", count)
			}
			// 子を明示的に片付ければ DELETE できる。FK は親の DELETE 自体を禁止する trigger ではない。
			if _, err := pool.Exec(ctx, "DELETE FROM "+satellite+" WHERE "+key+" = $1", id); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, "DELETE FROM "+parent+" WHERE id = $1", id); err != nil {
				t.Fatalf("delete after explicit cleanup: %v", err)
			}
		})
	}
}
