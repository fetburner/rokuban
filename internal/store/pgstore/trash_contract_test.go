package pgstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var trashContractEventID atomic.Int32

func TestRecordingTrashContract(t *testing.T) {
	contracttest.RunTrash(t, func(t *testing.T) contracttest.TrashFixture {
		pool := testutil.SetupDB(t)
		return contracttest.TrashFixture{
			Trash: NewRecordings(pool, dbtest.StoreName), Purge: NewRecordings(pool, dbtest.StoreName),
			Serving: NewMedia(pool, dbtest.StoreName), Catalog: NewCatalog(pool, []string{store.DefaultSite}, dbtest.StoreName),
			NewRecording: func(t *testing.T, options contracttest.RecordingOptions) int64 {
				return newContractRecording(t, pool, options, &trashContractEventID)
			},
			SetDeletedAt: func(t *testing.T, recordingID int64, deletedAt time.Time) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = $2 WHERE id = $1`, recordingID, deletedAt); err != nil {
					t.Fatalf("setting deletion time: %v", err)
				}
			},
			BeginPurgeBlocker: func(t *testing.T, recordingID int64) (func(), func()) {
				return beginPurgeBlocker(t, pool, recordingID)
			},
			BeginRestoreBlocker: func(t *testing.T, recordingID int64) func() {
				return beginRestoreBlocker(t, pool, recordingID)
			},
			WaitForLockWaiter: func(t *testing.T, operation string) { waitForContractLockWaiter(t, pool, operation) },
		}
	})
}

func beginPurgeBlocker(t *testing.T, pool *pgxpool.Pool, recordingID int64) (func(), func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning purge blocker transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `UPDATE recordings SET updated_at = now() WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("locking recording for purge blocker: %v", err)
	}
	var commitOnce sync.Once
	commit := func() {
		commitOnce.Do(func() {
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("committing purge blocker transaction: %v", err)
			}
		})
	}
	insertRequest := func() {
		if _, err := tx.Exec(ctx, `INSERT INTO recording_purge_requests (recording_id) VALUES ($1)`, recordingID); err != nil {
			t.Fatalf("inserting concurrent purge request: %v", err)
		}
	}
	return insertRequest, commit
}

func beginRestoreBlocker(t *testing.T, pool *pgxpool.Pool, recordingID int64) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning restore blocker transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `UPDATE recordings SET deleted_at = NULL, updated_at = now() WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("locking recording for restore blocker: %v", err)
	}
	var commitOnce sync.Once
	return func() {
		commitOnce.Do(func() {
			if _, err := tx.Exec(ctx, `DELETE FROM recording_purge_requests WHERE recording_id = $1`, recordingID); err != nil {
				t.Errorf("withdrawing purge request in restore blocker: %v", err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("committing restore blocker transaction: %v", err)
			}
		})
	}
}

func waitForContractLockWaiter(t *testing.T, pool *pgxpool.Pool, operation string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
  AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatalf("polling pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not wait for a row lock", operation)
}
