package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var tsScanContractEventID atomic.Int32

func TestTSScanStoreContract(t *testing.T) {
	contracttest.RunTSScan(t, newTSScanContractFixture)
}

func newTSScanContractFixture(t *testing.T) contracttest.TSScanFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	return contracttest.TSScanFixture{
		Store: NewMedia(pool),
		NewOriginal: func(t *testing.T, sizeBytes int64, trashed bool) (int64, int64) {
			t.Helper()
			ctx := context.Background()
			recordingID, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				Source:            "manual",
				Site:              db.DefaultSite,
				NetworkID:         32678,
				ServiceID:         5168,
				EventID:           tsScanContractEventID.Add(1),
				ServiceName:       "TS scan store contract fixture",
				ChannelType:       "GR",
				Channel:           "27",
				Title:             "TS scan store contract fixture",
				ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
				ProgramDurationMs: 60_000,
				Status:            "finished",
			})
			if err != nil {
				t.Fatalf("seeding recording: %v", err)
			}
			assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
				RecordingID: recordingID,
				Kind:        db.AssetKindOriginal,
				RelPath:     fmt.Sprintf("sites/%s/ts-scan-contract/%d.ts", db.DefaultSite, recordingID),
				SizeBytes:   sizeBytes,
			})
			if err != nil {
				t.Fatalf("seeding original media asset: %v", err)
			}
			if trashed {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID); err != nil {
					t.Fatalf("trashing recording: %v", err)
				}
			}
			return recordingID, assetID
		},
		NewEncodedOnlyRecording: func(t *testing.T) (int64, int64) {
			t.Helper()
			ctx := context.Background()
			recordingID, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				Source:            "manual",
				Site:              db.DefaultSite,
				NetworkID:         32678,
				ServiceID:         5168,
				EventID:           tsScanContractEventID.Add(1),
				ServiceName:       "TS scan store contract encoded-only fixture",
				ChannelType:       "GR",
				Channel:           "27",
				Title:             "TS scan store contract encoded-only fixture",
				ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
				ProgramDurationMs: 60_000,
				Status:            "finished",
			})
			if err != nil {
				t.Fatalf("seeding encoded-only recording: %v", err)
			}
			profile := "h264"
			assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
				RecordingID: recordingID,
				Kind:        db.AssetKindEncoded,
				Profile:     &profile,
				RelPath:     fmt.Sprintf("sites/%s/ts-scan-contract/%d.mp4", db.DefaultSite, recordingID),
				SizeBytes:   1,
			})
			if err != nil {
				t.Fatalf("seeding encoded-only media asset: %v", err)
			}
			return recordingID, assetID
		},
		SetAssetSize: func(t *testing.T, assetID, sizeBytes int64) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET size_bytes = $2 WHERE id = $1`, assetID, sizeBytes); err != nil {
				t.Fatalf("setting media asset size: %v", err)
			}
		},
		SetAssetState: func(t *testing.T, assetID int64, state string) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET state = $2 WHERE id = $1`, assetID, state); err != nil {
				t.Fatalf("setting media asset state: %v", err)
			}
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
		ReadSnapshot: func(t *testing.T, assetID int64) contracttest.TSScanSnapshot {
			t.Helper()
			ctx := context.Background()
			snapshot := contracttest.TSScanSnapshot{Stats: make([]store.RecordingDropStat, 0), Positions: make([]store.RecordingDropPosition, 0), ScannedSizeBytes: -1}
			rows, err := pool.Query(ctx, `
SELECT pid, packets, drops, errors, scrambled, pid_type
FROM drop_stats WHERE media_asset_id = $1 ORDER BY pid`, assetID)
			if err != nil {
				t.Fatalf("reading drop_stats: %v", err)
			}
			for rows.Next() {
				var stat store.RecordingDropStat
				if err := rows.Scan(&stat.Pid, &stat.Packets, &stat.Drops, &stat.Errors, &stat.Scrambled, &stat.PidType); err != nil {
					rows.Close()
					t.Fatalf("scanning drop_stats: %v", err)
				}
				snapshot.Stats = append(snapshot.Stats, stat)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatalf("iterating drop_stats: %v", err)
			}
			rows.Close()

			rows, err = pool.Query(ctx, `
SELECT pid, byte_offset, elapsed_ms
FROM drop_positions WHERE media_asset_id = $1 ORDER BY byte_offset`, assetID)
			if err != nil {
				t.Fatalf("reading drop_positions: %v", err)
			}
			for rows.Next() {
				var position store.RecordingDropPosition
				if err := rows.Scan(&position.Pid, &position.ByteOffset, &position.ElapsedMs); err != nil {
					rows.Close()
					t.Fatalf("scanning drop_positions: %v", err)
				}
				snapshot.Positions = append(snapshot.Positions, position)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatalf("iterating drop_positions: %v", err)
			}
			rows.Close()

			if err := pool.QueryRow(ctx, `SELECT scanned_size_bytes FROM media_asset_ts_scans WHERE media_asset_id = $1`, assetID).Scan(&snapshot.ScannedSizeBytes); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("reading TS scan marker: %v", err)
			}
			return snapshot
		},
		BeginAssetStateChange: func(t *testing.T, assetID int64, state string) func() {
			t.Helper()
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("beginning media asset state change: %v", err)
			}
			t.Cleanup(func() { _ = tx.Rollback(ctx) })
			if _, err := tx.Exec(ctx, `UPDATE media_assets SET state = $2 WHERE id = $1`, assetID, state); err != nil {
				t.Fatalf("changing media asset state in blocker transaction: %v", err)
			}
			var commitOnce sync.Once
			return func() {
				commitOnce.Do(func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("committing media asset state change: %v", err)
					}
				})
			}
		},
		WaitForLockWaiter: func(t *testing.T, operation string) {
			t.Helper()
			waitForContractLockWaiter(t, pool, operation)
		},
	}
}
