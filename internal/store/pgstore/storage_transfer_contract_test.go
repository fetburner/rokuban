package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestStorageTransferStoreContract(t *testing.T) {
	contracttest.RunStorageTransfer(t, func(t *testing.T) contracttest.StorageTransferFixture {
		pool := testutil.SetupDB(t)
		return contracttest.StorageTransferFixture{
			Store:       testMedia(pool, "test-store", ""),
			LoopMarkers: NewReservations(pool),
			WithPolicy: func(storeName string, rules []store.OriginalStoreRule, fsStores []string, derivedStore string) store.StorageTransfer {
				policy := store.NewStoragePolicy(rules, fsStores, derivedStore).WithPlacementEnabled(true)
				return NewMediaInStore(pool, storeName, policy)
			},
			RejectTransferCompletionMarker: func(t *testing.T) func() {
				t.Helper()
				execTransferFixture(t, pool, `ALTER TABLE loop_success_markers
ADD CONSTRAINT reject_transfer_marker CHECK (loop <> 'storage_transfer')`)
				return func() {
					t.Helper()
					execTransferFixture(t, pool, `ALTER TABLE loop_success_markers DROP CONSTRAINT reject_transfer_marker`)
				}
			},
			NewPendingCopy: func(t *testing.T) (int64, int64) {
				t.Helper()
				return progressTestCopy(t, pool)
			},
			ExpireProgressLease: func(t *testing.T, copyID int64) {
				t.Helper()
				expireProgressLease(t, pool, copyID)
			},
			DeleteCopy: func(t *testing.T, copyID int64) {
				t.Helper()
				execTransferFixture(t, pool, `DELETE FROM media_asset_copies WHERE id = $1`, copyID)
			},
			BeginCopyRetirement: func(t *testing.T, copyID int64) func() {
				t.Helper()
				return holdTransferFixture(t, pool, `UPDATE media_asset_copies
SET state = 'deleted', deleted_at = now() WHERE id = $1`, copyID)
			},
			BeginBacklogWriteHold: func(t *testing.T) func() {
				t.Helper()
				return holdTransferFixture(t, pool, `LOCK TABLE transfer_backlog_snapshots IN SHARE MODE`)
			},
			WaitForBlockedTransfers: func(t *testing.T, count int, finished <-chan struct{}) {
				t.Helper()
				waitForTransferContractWaiters(t, pool, count, finished)
			},
			NewRecording: func(t *testing.T, site string, programStartAt time.Time) int64 {
				t.Helper()
				return dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
					params.Site = site
					if !programStartAt.IsZero() {
						params.ProgramStartAt = programStartAt
					}
				})
			},
			TrashRecording: func(t *testing.T, recordingID int64) {
				t.Helper()
				execTransferFixture(t, pool, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, recordingID)
			},
			NewAsset: func(t *testing.T, recordingID int64, kind, sourceStore, pathPrefix string) (int64, int64) {
				t.Helper()
				assetID := dbtest.MediaAsset(t, pool, recordingID, func(params *dbtest.MediaAssetParams) {
					if kind != "" {
						params.Kind = kind
					}
					if kind == "encoded" {
						profile := "web"
						params.Profile = &profile
					}
				})
				var copyID int64
				if err := pool.QueryRow(t.Context(), `
UPDATE media_asset_copies
SET store = $2, rel_path = $3::text || '/' || $1::text || '/' || $2 || '.ts'
WHERE media_asset_id = $1 AND state = 'active'
RETURNING id`, assetID, sourceStore, pathPrefix).Scan(&copyID); err != nil {
					t.Fatalf("placing active copy for asset %d in %q: %v", assetID, sourceStore, err)
				}
				return assetID, copyID
			},
			AddCopy: func(t *testing.T, assetID int64, storeName, relPath, state string, generation int64) int64 {
				t.Helper()
				var copyID int64
				if err := pool.QueryRow(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, $2, $3, $4, $5) RETURNING id`, assetID, storeName, relPath, generation, state).Scan(&copyID); err != nil {
					t.Fatalf("adding %s copy for asset %d in %q: %v", state, assetID, storeName, err)
				}
				return copyID
			},
			MarkCopyMissing: func(t *testing.T, copyID int64) {
				t.Helper()
				execTransferFixture(t, pool, `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, copyID)
			},
			SeedTransferAttempts: func(t *testing.T, copyID int64, count int32, message string) {
				t.Helper()
				execTransferFixture(t, pool, `INSERT INTO media_asset_transfer_attempts (copy_id, attempt_count, last_error)
VALUES ($1, $2, $3)`, copyID, count, message)
			},
		}
	})
}

func execTransferFixture(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("setting up transfer fixture: %v", err)
	}
}

func holdTransferFixture(t *testing.T, pool *pgxpool.Pool, query string, args ...any) func() {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning transfer fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatalf("holding transfer fixture transaction: %v", err)
	}
	return func() {
		t.Helper()
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("committing transfer fixture transaction: %v", err)
		}
	}
}

// waitForTransferContractWaiters はテストの transaction が重なった時点を検出する。
// finished が閉じた場合は操作が待たずに終わっているので、呼び出し元は結果で判定する。
func waitForTransferContractWaiters(t *testing.T, pool *pgxpool.Pool, count int, finished <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// 並行実行側が pool の接続をすべて借りていても観測できるよう、別の接続を使う。
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connecting to observe transfer waiters: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := conn.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("polling transfer fixture waiters: %v", err)
		}
		if waiting >= count {
			return
		}
		select {
		case <-finished:
			return
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("only %d operations waiting for the fixture, want %d", waiting, count)
		}
	}
}
