package pgstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var mediaDeleteContractEventID atomic.Int32

//nolint:funlen // 契約の各フックを 1 か所に並べる表。分割すると、フックと状態作りの対応が追えなくなる。
func TestMediaDeleteStoreContract(t *testing.T) {
	contracttest.RunMediaDelete(t, func(t *testing.T) contracttest.MediaDeleteFixture {
		pool := testutil.SetupDB(t)
		newRecording := func(t *testing.T) int64 {
			t.Helper()
			id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
				fixtureParams.EventID = mediaDeleteContractEventID.Add(1)
				fixtureParams.ServiceName = "store media delete contract fixture"
				fixtureParams.Title = "store media delete contract fixture"
				fixtureParams.ProgramDurationMs = 60_000
			})

			return id
		}
		newAsset := func(t *testing.T, recordingID int64, kind string, profile *string) int64 {
			t.Helper()
			id := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
				fixtureParams.Kind = kind
				fixtureParams.Profile = profile
				fixtureParams.RelPath = fmt.Sprintf("sites/%s/store-contract/%d/%s", store.DefaultSite, recordingID, kind)
			})

			return id
		}
		storage := testMedia(pool, dbtest.StoreName, "")
		return contracttest.MediaDeleteFixture{
			Store:      testMedia(pool, dbtest.StoreName, dbtest.StoreName),
			BoundStore: dbtest.StoreName,
			StoreWithDerived: func(derivedStore string) store.MediaDelete {
				return testMedia(pool, dbtest.StoreName, derivedStore)
			},
			StoreWithPlacement: func(rules []store.OriginalStoreRule, derivedStore string) store.MediaDelete {
				return NewMediaInStore(pool, dbtest.StoreName, store.NewStoragePolicy(rules, nil, derivedStore).WithPlacementEnabled(true))
			},
			Serving: storage,
			Catalog: NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName),
			MarkMissing: func(t *testing.T, copyID int64) {
				t.Helper()
				if err := storage.UpsertMissingMediaAsset(t.Context(), copyID); err != nil {
					t.Fatalf("reporting missing copy %d: %v", copyID, err)
				}
			},
			NewOriginal: func(t *testing.T) (int64, int64) {
				t.Helper()
				recordingID := newRecording(t)
				return recordingID, newAsset(t, recordingID, store.AssetKindOriginal, nil)
			},
			SetAssetState: func(t *testing.T, assetID int64, state string) {
				dbtest.SetMediaAssetState(t, pool, assetID, state)
			},
			NewOldGeneration: func(t *testing.T) (int64, int64) {
				t.Helper()
				recordingID := newRecording(t)
				assetID := newAsset(t, recordingID, "thumbnail", nil)
				oldCopy := dbtest.MediaAssetCopyID(t, pool, assetID, dbtest.StoreName)
				path := fmt.Sprintf("thumbnails/%d-g1.jpg", recordingID)
				current := dbtest.ReplaceDerivedMediaAssetCopy(t, pool, assetID, dbtest.StoreName, path, 1)
				return oldCopy, current
			},
			NewRetainedEncoded: func(t *testing.T, recordingID int64) int64 {
				t.Helper()
				if _, err := pool.Exec(t.Context(), `
UPDATE recording_processing_policy SET encode_profiles = ARRAY['p2'] WHERE recording_id = $1`, recordingID); err != nil {
					t.Fatalf("adding retained profile: %v", err)
				}
				return dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
					profile := "p2"
					p.Kind, p.Profile = "encoded", &profile
					p.RelPath = fmt.Sprintf("sites/default/store-contract/%d/p2.mp4", recordingID)
				})
			},
			CopyID: func(t *testing.T, assetID int64) int64 {
				t.Helper()
				return dbtest.MediaAssetCopyID(t, pool, assetID, dbtest.StoreName)
			},
			CopyState: func(t *testing.T, copyID int64) string {
				t.Helper()
				var state string
				if err := pool.QueryRow(t.Context(), `SELECT state FROM media_asset_copies WHERE id = $1`, copyID).Scan(&state); err != nil {
					t.Fatalf("reading state of copy %d: %v", copyID, err)
				}
				return state
			},
			MoveCopy: func(t *testing.T, assetID int64, from, to string) int64 {
				t.Helper()
				if _, err := pool.Exec(t.Context(),
					`UPDATE media_asset_copies SET store = $3 WHERE media_asset_id = $1 AND store = $2`,
					assetID, from, to); err != nil {
					t.Fatalf("moving copy of asset %d from %s to %s: %v", assetID, from, to, err)
				}
				return dbtest.MediaAssetCopyID(t, pool, assetID, to)
			},
			AdvanceDerivedGeneration: func(t *testing.T, assetID int64) {
				t.Helper()
				if _, err := pool.Exec(t.Context(), `UPDATE media_assets SET derived_generation = derived_generation + 1 WHERE id = $1`, assetID); err != nil {
					t.Fatalf("advancing derived generation of asset %d: %v", assetID, err)
				}
			},
			AddCurrentCopy: func(t *testing.T, assetID int64, storeName string) int64 {
				t.Helper()
				var copyID int64
				if err := pool.QueryRow(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT id, $2, $3, derived_generation, 'active' FROM media_assets WHERE id = $1
RETURNING id`, assetID, storeName, fmt.Sprintf("sites/%s/store-contract/current/%d/%s", store.DefaultSite, assetID, storeName)).Scan(&copyID); err != nil {
					t.Fatalf("adding current copy of asset %d in %s: %v", assetID, storeName, err)
				}
				return copyID
			},
			AssetOfKind: func(t *testing.T, recordingID int64, kind string) int64 {
				t.Helper()
				var assetID int64
				if err := pool.QueryRow(t.Context(), `SELECT id FROM media_assets WHERE recording_id = $1 AND kind = $2`, recordingID, kind).Scan(&assetID); err != nil {
					t.Fatalf("reading %s asset of recording %d: %v", kind, recordingID, err)
				}
				return assetID
			},
			NewUntilEncodedOriginal: func(t *testing.T) (int64, int64) {
				t.Helper()
				recordingID := newRecording(t)
				originalID := newAsset(t, recordingID, store.AssetKindOriginal, nil)
				profile := "h264"
				newAsset(t, recordingID, store.AssetKindEncoded, &profile)
				newAsset(t, recordingID, store.AssetKindThumbnail, nil)
				newAsset(t, recordingID, store.AssetKindSeekTiles, nil)
				if _, err := pool.Exec(context.Background(), `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
SELECT id, size_bytes FROM media_assets WHERE id = $1`, originalID); err != nil {
					t.Fatalf("seeding original TS scan marker: %v", err)
				}
				if _, err := pool.Exec(context.Background(), `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'until_encoded', ARRAY['h264'], false)`, recordingID); err != nil {
					t.Fatalf("seeding until_encoded policy: %v", err)
				}
				return recordingID, originalID
			},
			BeginUntilEncodedInvalidation: func(t *testing.T, recordingID int64) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning rebuild request transaction: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback(ctx) })
				if _, err := tx.Exec(ctx, `UPDATE recordings SET updated_at = now() WHERE id = $1`, recordingID); err != nil {
					t.Fatalf("locking recording for rebuild request: %v", err)
				}
				if _, err := tx.Exec(ctx, `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
VALUES ($1, 'h264', '{[0,1000)}'::int8multirange)`, recordingID); err != nil {
					t.Fatalf("seeding pending rebuild request: %v", err)
				}
				return func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("committing rebuild request: %v", err)
					}
				}
			},
			BeginCopyDeletion: func(t *testing.T, copyID int64) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning copy deletion transaction: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback(ctx) })
				if _, err := tx.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE id = $1`, copyID); err != nil {
					t.Fatalf("marking copy %d deleted in the open transaction: %v", copyID, err)
				}
				return func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("committing copy deletion: %v", err)
					}
				}
			},
			NewDeletedAsset: func(t *testing.T) (int64, int64) {
				t.Helper()
				recordingID := newRecording(t)
				assetID := newAsset(t, recordingID, store.AssetKindOriginal, nil)
				dbtest.SetMediaAssetState(t, pool, assetID, "deleted")
				return recordingID, assetID
			},
			NewRemovedEncoded: func(t *testing.T) (int64, int64) {
				t.Helper()
				recordingID := newRecording(t)
				newAsset(t, recordingID, store.AssetKindOriginal, nil)
				if _, err := pool.Exec(context.Background(), `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', false)`, recordingID); err != nil {
					t.Fatalf("seeding policy without the removed profile: %v", err)
				}
				profile := "h264"
				assetID := newAsset(t, recordingID, store.AssetKindEncoded, &profile)
				if err := sqlcgen.New(pool).InsertEncodedAssetRemovalRequest(context.Background(), sqlcgen.InsertEncodedAssetRemovalRequestParams{
					RecordingID: recordingID, Profile: profile,
				}); err != nil {
					t.Fatalf("inserting removal request: %v", err)
				}
				return recordingID, assetID
			},
			TrashRecording: func(t *testing.T, recordingID int64, deletedAt time.Time, purgeRequested bool) {
				t.Helper()
				if _, err := pool.Exec(t.Context(), `UPDATE recordings SET deleted_at = $2 WHERE id = $1`, recordingID, deletedAt); err != nil {
					t.Fatalf("trashing recording %d: %v", recordingID, err)
				}
				if purgeRequested {
					if _, err := pool.Exec(t.Context(), `INSERT INTO recording_purge_requests (recording_id) VALUES ($1)`, recordingID); err != nil {
						t.Fatalf("requesting purge of recording %d: %v", recordingID, err)
					}
				}
			},
			RemovalRequestExists: func(t *testing.T, recordingID int64, profile string) bool {
				t.Helper()
				var exists bool
				if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM encoded_asset_removal_requests WHERE recording_id = $1 AND profile = $2
)`, recordingID, profile).Scan(&exists); err != nil {
					t.Fatalf("reading encoded asset removal request: %v", err)
				}
				return exists
			},
			WaitForLockWaiter: func(t *testing.T, operation string) {
				t.Helper()
				waitForContractLockWaiter(t, pool, operation)
			},
		}
	})
}
