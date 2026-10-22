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

func TestMediaDeleteStoreContract(t *testing.T) {
	contracttest.RunMediaDelete(t, func(t *testing.T) contracttest.MediaDeleteFixture {
		pool := testutil.SetupDB(t)
		newRecording := func(t *testing.T) int64 {
			t.Helper()
			id := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
				fixtureParams.EventID = mediaDeleteContractEventID.Add(1)
				fixtureParams.ServiceName = "store media delete contract fixture"
				fixtureParams.Title = "store media delete contract fixture"
				fixtureParams.ProgramStartAt = time.Now().UTC().Truncate(time.Second)
				fixtureParams.ProgramDurationMs = 60_000
			})

			return id
		}
		newAsset := func(t *testing.T, recordingID int64, kind string, profile *string) int64 {
			t.Helper()
			id := dbtest.OriginalAsset(t, pool, recordingID, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
				fixtureParams.Kind = kind
				fixtureParams.Profile = profile
				fixtureParams.RelPath = fmt.Sprintf("sites/%s/store-contract/%d/%s", store.DefaultSite, recordingID, kind)
				fixtureParams.SizeBytes = 1
			})

			return id
		}
		return contracttest.MediaDeleteFixture{
			Store: NewMedia(pool),
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
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
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
			NewDeletedAsset: func(t *testing.T) int64 {
				t.Helper()
				assetID := newAsset(t, newRecording(t), store.AssetKindOriginal, nil)
				if _, err := pool.Exec(context.Background(), `
UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1`, assetID); err != nil {
					t.Fatalf("marking media asset deleted: %v", err)
				}
				return assetID
			},
			ReadAssetState: func(t *testing.T, assetID int64) string {
				t.Helper()
				var state string
				if err := pool.QueryRow(context.Background(), `SELECT state FROM media_assets WHERE id = $1`, assetID).Scan(&state); err != nil {
					t.Fatalf("reading media asset state: %v", err)
				}
				return state
			},
			NewDeletingRemovedEncoded: func(t *testing.T) int64 {
				t.Helper()
				recordingID := newRecording(t)
				profile := "h264"
				assetID := newAsset(t, recordingID, store.AssetKindEncoded, &profile)
				if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET state = 'deleting' WHERE id = $1`, assetID); err != nil {
					t.Fatalf("marking media asset deleting: %v", err)
				}
				if err := sqlcgen.New(pool).InsertEncodedAssetRemovalRequest(context.Background(), sqlcgen.InsertEncodedAssetRemovalRequestParams{
					RecordingID: recordingID, Profile: profile,
				}); err != nil {
					t.Fatalf("inserting removal request: %v", err)
				}
				return assetID
			},
			RemovalRequestExists: func(t *testing.T, assetID int64) bool {
				t.Helper()
				var exists bool
				if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM encoded_asset_removal_requests r
  JOIN media_assets a ON a.recording_id = r.recording_id AND a.profile = r.profile
  WHERE a.id = $1)`, assetID).Scan(&exists); err != nil {
					t.Fatalf("querying removal request: %v", err)
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
