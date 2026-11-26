package mysqlstore

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

var trashContractEventSeq atomic.Int32

func TestRecordingTrashContract(t *testing.T) {
	contracttest.RunTrash(t, func(t *testing.T) contracttest.TrashFixture {
		db := mysqltest.SetupDB(t)
		return contracttest.TrashFixture{
			Trash:   NewRecordings(db),
			Purge:   NewRecordings(db),
			Serving: NewMediaInStore(db, "tokyo", store.StoragePolicy{}),
			Catalog: NewCatalog(db, []string{store.DefaultSite}, "tokyo"),
			NewRecording: func(t *testing.T, options contracttest.RecordingOptions) int64 {
				return newContractRecording(t, db, options, &trashContractEventSeq)
			},
			SetDeletedAt: func(t *testing.T, recordingID int64, deletedAt time.Time) {
				t.Helper()
				execFixture(t, db, `UPDATE recordings SET deleted_at = ? WHERE id = ?`, deletedAt.UTC(), recordingID)
			},
			BeginPurgeBlocker: func(t *testing.T, recordingID int64) (func(), func()) {
				return beginTrashPurgeBlocker(t, db, recordingID)
			},
			BeginRestoreBlocker: func(t *testing.T, recordingID int64) func() {
				return beginTrashRestoreBlocker(t, db, recordingID)
			},
			WaitForLockWaiter: func(t *testing.T, operation string) { waitForLockWaiter(t, db, operation) },
		}
	})
}

// beginTrashPurgeBlocker は録画の行をロックする tx を開き、要求行を入れる操作と commit を返す。
func beginTrashPurgeBlocker(t *testing.T, db *sql.DB, recordingID int64) (func(), func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning purge blocker transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if err := lockRecordingState(ctx, tx, recordingID); err != nil {
		t.Fatalf("locking recording for purge blocker: %v", err)
	}
	var commitOnce sync.Once
	commit := func() {
		commitOnce.Do(func() {
			if err := tx.Commit(); err != nil {
				t.Errorf("committing purge blocker transaction: %v", err)
			}
		})
	}
	insertRequest := func() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recording_purge_requests (recording_id) VALUES (?)`, recordingID); err != nil {
			t.Fatalf("inserting concurrent purge request: %v", err)
		}
	}
	return insertRequest, commit
}

// beginTrashRestoreBlocker は復元後の録画の状態を未 commit で作り、要求の取り下げと commit を行う操作を返す。
func beginTrashRestoreBlocker(t *testing.T, db *sql.DB, recordingID int64) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning restore blocker transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `UPDATE recordings SET deleted_at = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, recordingID); err != nil {
		t.Fatalf("locking recording for restore blocker: %v", err)
	}
	var commitOnce sync.Once
	return func() {
		commitOnce.Do(func() {
			if _, err := tx.ExecContext(ctx, `DELETE FROM recording_purge_requests WHERE recording_id = ?`, recordingID); err != nil {
				t.Errorf("withdrawing purge request in restore blocker: %v", err)
				return
			}
			if err := tx.Commit(); err != nil {
				t.Errorf("committing restore blocker transaction: %v", err)
			}
		})
	}
}
