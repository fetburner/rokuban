package pgstore

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestTSScanStoreContract(t *testing.T) {
	contracttest.RunTSScan(t, newTSScanContractFixture)
}

func newTSScanContractFixture(t *testing.T) contracttest.TSScanFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	return contracttest.TSScanFixture{
		Store:   NewMedia(pool, dbtest.StoreName),
		Catalog: NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName),
		NewOriginal: func(t *testing.T, sizeBytes int64, trashed bool) (int64, int64) {
			t.Helper()
			ctx := context.Background()
			recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.Title = "TS scan store contract fixture"
				p.ProgramDurationMs = 60_000
			})
			assetID := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
				fixtureParams.RelPath = fmt.Sprintf("sites/%s/ts-scan-contract/%d.ts", store.DefaultSite, recordingID)
				fixtureParams.SizeBytes = sizeBytes
			})

			if trashed {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID); err != nil {
					t.Fatalf("trashing recording: %v", err)
				}
			}
			return recordingID, assetID
		},
		NewEncodedOnlyRecording: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.Title = "TS scan store contract encoded-only fixture"
				p.ProgramDurationMs = 60_000
			})
			profile := "h264"
			assetID := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
				fixtureParams.Kind = store.AssetKindEncoded
				fixtureParams.Profile = &profile
				fixtureParams.RelPath = fmt.Sprintf("sites/%s/ts-scan-contract/%d.mp4", store.DefaultSite, recordingID)
			})

			return recordingID, assetID
		},
		SetAssetSize: func(t *testing.T, assetID, sizeBytes int64) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET size_bytes = $2 WHERE id = $1`, assetID, sizeBytes); err != nil {
				t.Fatalf("setting media asset size: %v", err)
			}
		},
		SetAssetState: func(t *testing.T, assetID int64, state string) {
			dbtest.SetMediaAssetState(t, pool, assetID, state)
		},
		SetScannedSize: func(t *testing.T, assetID, scannedSizeBytes int64) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
VALUES ($1, $2)
ON CONFLICT (media_asset_id) DO UPDATE SET scanned_size_bytes = EXCLUDED.scanned_size_bytes`, assetID, scannedSizeBytes); err != nil {
				t.Fatalf("setting TS scan marker: %v", err)
			}
		},
		SeedSnapshot: func(t *testing.T, assetID int64, snapshot contracttest.TSScanSnapshot) {
			t.Helper()
			ctx := context.Background()
			for _, stat := range snapshot.Stats {
				if _, err := pool.Exec(ctx, `
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, assetID, stat.Pid, stat.Packets, stat.Drops, stat.Errors, stat.Scrambled, stat.PidType); err != nil {
					t.Fatalf("seeding drop_stats: %v", err)
				}
			}
			for _, position := range snapshot.Positions {
				if _, err := pool.Exec(ctx, `
INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms)
VALUES ($1, $2, $3, $4)`, assetID, position.ByteOffset, position.Pid, position.ElapsedMs); err != nil {
					t.Fatalf("seeding drop_positions: %v", err)
				}
			}
			if snapshot.ScannedSizeBytes >= 0 {
				if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes) VALUES ($1, $2)`, assetID, snapshot.ScannedSizeBytes); err != nil {
					t.Fatalf("seeding TS scan marker: %v", err)
				}
			}
		},
		BeginAssetStateChange: func(t *testing.T, assetID int64, state string) func() {
			return beginTSScanContractAssetStateChange(t, pool, assetID, state)
		},
		WaitForLockWaiter: func(t *testing.T, operation string) {
			t.Helper()
			waitForContractLockWaiter(t, pool, operation)
		},
	}
}

func beginTSScanContractAssetStateChange(t *testing.T, pool *pgxpool.Pool, assetID int64, state string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning media asset state change: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	dbtest.SetMediaAssetState(t, tx, assetID, state)
	var commitOnce sync.Once
	return func() {
		commitOnce.Do(func() {
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("committing media asset state change: %v", err)
			}
		})
	}
}
