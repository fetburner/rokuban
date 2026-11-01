package pgstore

import (
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// storage_transfer_pending_copy_obsolete は、予約の pending copy が宛先と食い違うときだけ真を返す。
// 規則は tiered を archive へ、remote を FS 外の remote へ送る。
func TestStorageTransferPendingCopyObsolete(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	rules := `[{"match":{"site":"tiered"},"store":"archive"},{"match":{"site":"remote"},"store":"remote"}]`
	fsStores := []string{"edge", "backup", "archive"}

	tests := []struct {
		name      string
		site      string
		copyStore string
		deleted   bool
		want      bool
	}{
		{name: "deleted recording makes the reservation obsolete", site: "tiered", copyStore: "archive", deleted: true, want: true},
		{name: "desired store outside FS makes the reservation obsolete", site: "remote", copyStore: "archive", want: true},
		{name: "another desired store makes the reservation obsolete", site: "tiered", copyStore: "backup", want: true},
		{name: "matching desired store keeps the reservation", site: "tiered", copyStore: "archive", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recordingID := dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
				params.Site = tc.site
			})
			assetID := dbtest.MediaAsset(t, pool, recordingID)
			if _, err := pool.Exec(ctx, `
UPDATE media_asset_copies
SET store = 'edge', rel_path = 'obsolete-test/' || $1::text || '/edge.ts'
WHERE media_asset_id = $1 AND state = 'active'`, assetID); err != nil {
				t.Fatalf("placing active source copy for asset %d: %v", assetID, err)
			}
			var copyID int64
			if err := pool.QueryRow(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, $2, $3, 0, 'pending')
RETURNING id`, assetID, tc.copyStore, fmt.Sprintf("obsolete-test/%d/%s.ts", assetID, tc.copyStore)).Scan(&copyID); err != nil {
				t.Fatalf("adding pending %q copy for asset %d: %v", tc.copyStore, assetID, err)
			}
			if tc.deleted {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID); err != nil {
					t.Fatalf("deleting recording %d: %v", recordingID, err)
				}
			}
			var got bool
			if err := pool.QueryRow(ctx,
				`SELECT storage_transfer_pending_copy_obsolete($1, $2::jsonb, $3::text[], $4::text)`,
				copyID, rules, fsStores, "derived",
			).Scan(&got); err != nil {
				t.Fatalf("storage_transfer_pending_copy_obsolete: %v", err)
			}
			if got != tc.want {
				t.Fatalf("obsolete = %v, want %v", got, tc.want)
			}
		})
	}
}
