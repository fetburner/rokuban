package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

var transferContractEventID atomic.Int32
var transferContractAssetPathID atomic.Int64

func TestStorageTransferStoreContract(t *testing.T) {
	t.Parallel()
	contracttest.RunStorageTransfer(t, newStorageTransferContractFixture)
}

func newStorageTransferContractFixture(t *testing.T) contracttest.StorageTransferFixture {
	db := mysqltest.SetupDB(t)
	fixture := contracttest.StorageTransferFixture{
		Store:       NewMediaInStore(db, "test-store", store.StoragePolicy{}),
		LoopMarkers: NewLoopMarkers(db),
		WithPolicy: func(storeName string, rules []store.OriginalStoreRule, fsStores []string, derivedStore string) store.StorageTransfer {
			policy := store.NewStoragePolicy(rules, fsStores, derivedStore).WithPlacementEnabled(true)
			if storeName == "" {
				return NewMediaUnboundWithPolicy(db, policy)
			}
			return NewMediaInStore(db, storeName, policy)
		},
		NewPendingCopy: func(t *testing.T) (copyID, assetID int64) {
			t.Helper()
			recordingID := newTransferContractRecording(t, db, "default", time.Time{})
			assetID = fixtureID(t, execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 1)`, recordingID))
			transferContractCopy(t, db, assetID, "test-store", fmt.Sprintf("test-original/%d.ts", transferContractAssetPathID.Add(1)), "active", 0)
			copyID = transferContractCopy(t, db, assetID, "archive", fmt.Sprintf("progress/%d.ts", assetID), "pending", 0)
			return copyID, assetID
		},
		ExpireProgressLease: func(t *testing.T, copyID int64) {
			t.Helper()
			execFixture(t, db, `UPDATE media_asset_copy_transfer_progress
SET lease_expires_at = CURRENT_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE copy_id = ?`, copyID)
		},
		DeleteCopy: func(t *testing.T, copyID int64) {
			t.Helper()
			// MySQL の progress FK は RESTRICT で、FK CASCADE では DELETE trigger が発火しない。
			// 明示的に進捗を削除してからコピーを削除し、放棄記録は進捗表の trigger に作らせる。
			commit := beginFixtureTx(t, db, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `DELETE FROM media_asset_copy_transfer_progress WHERE copy_id = ?`, copyID); err != nil {
					return err
				}
				_, err := tx.ExecContext(t.Context(), `DELETE FROM media_asset_copies WHERE id = ?`, copyID)
				return err
			})
			commit()
		},
		BeginCopyRetirement: func(t *testing.T, copyID int64) func() {
			t.Helper()
			return beginFixtureTx(t, db, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `UPDATE media_asset_copies
SET state = 'deleted', deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, copyID)
				return err
			})
		},
		BeginBacklogWriteHold: func(t *testing.T) func() {
			t.Helper()
			ctx := t.Context()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("borrowing connection for backlog hold: %v", err)
			}
			tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
			if err != nil {
				_ = conn.Close()
				t.Fatalf("beginning repeatable-read backlog hold: %v", err)
			}
			t.Cleanup(func() {
				_ = tx.Rollback()
				_ = conn.Close()
			})
			// REPEATABLE READ では、空の主キー索引に対するこのロック読み取りが supremum gap を保持する。
			// RecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce でこの待ちを検出する。
			rows, err := tx.QueryContext(ctx, `SELECT store FROM transfer_backlog_snapshots FOR SHARE`)
			if err != nil {
				t.Fatalf("holding transfer backlog snapshot range: %v", err)
			}
			for rows.Next() {
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				t.Fatalf("reading transfer backlog snapshots for hold: %v", err)
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("closing transfer backlog snapshot hold rows: %v", err)
			}
			var once sync.Once
			return func() {
				once.Do(func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("committing transfer backlog hold: %v", err)
					}
					if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
						t.Errorf("returning transfer backlog hold connection: %v", err)
					}
				})
			}
		},
		WaitForBlockedTransfers: func(t *testing.T, count int, finished <-chan struct{}) {
			t.Helper()
			waitForTransferLockWaiters(t, db, count, finished)
		},
		NewRecording: func(t *testing.T, site string, programStartAt time.Time) int64 {
			t.Helper()
			return newTransferContractRecording(t, db, site, programStartAt)
		},
		TrashRecording: func(t *testing.T, recordingID int64) {
			t.Helper()
			execFixture(t, db, `UPDATE recordings SET deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, recordingID)
		},
		NewAsset: func(t *testing.T, recordingID int64, kind, sourceStore, pathPrefix string) (assetID, copyID int64) {
			t.Helper()
			if kind == "" {
				kind = "original"
			}
			var profile any
			if kind == "encoded" {
				profile = "web"
			}
			assetID = fixtureID(t, execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, profile, size_bytes)
VALUES (?, ?, ?, 1)`, recordingID, kind, profile))
			path := fmt.Sprintf("%s/%d/%s.ts", pathPrefix, assetID, sourceStore)
			copyID = transferContractCopy(t, db, assetID, sourceStore, path, "active", 0)
			return assetID, copyID
		},
		AddCopy: func(t *testing.T, assetID int64, storeName, relPath, state string, generation int64) int64 {
			t.Helper()
			return transferContractCopy(t, db, assetID, storeName, relPath, state, generation)
		},
		MarkCopyMissing: func(t *testing.T, copyID int64) {
			t.Helper()
			execFixture(t, db, `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES (?)`, copyID)
		},
		SeedTransferAttempts: func(t *testing.T, copyID int64, count int32, message string) {
			t.Helper()
			execFixture(t, db, `INSERT INTO media_asset_transfer_attempts (copy_id, attempt_count, last_error) VALUES (?, ?, ?)`, copyID, count, message)
		},
	}
	configureStorageTransferFixtureMySQL(db, &fixture)
	return fixture
}

func configureStorageTransferFixtureMySQL(db *sql.DB, fixture *contracttest.StorageTransferFixture) {
	fixture.RejectTransferCompletionMarker = func(t *testing.T) func() {
		t.Helper()
		for _, trigger := range []struct{ name, event string }{
			{"reject_transfer_marker_insert", "INSERT"},
			{"reject_transfer_marker_update", "UPDATE"},
		} {
			_, err := db.ExecContext(t.Context(), `CREATE TRIGGER `+trigger.name+` BEFORE `+trigger.event+` ON loop_success_markers FOR EACH ROW
BEGIN
    IF NEW.loop = 'storage_transfer' AND NEW.scope = 'all' THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'storage transfer marker rejected';
    END IF;
END`)
			if err != nil {
				t.Fatalf("creating storage transfer marker %s trigger: %v", trigger.event, err)
			}
		}
		var once sync.Once
		restore := func() {
			once.Do(func() {
				for _, name := range []string{"reject_transfer_marker_insert", "reject_transfer_marker_update"} {
					if _, err := db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS `+name); err != nil {
						t.Errorf("dropping storage transfer marker trigger %s: %v", name, err)
					}
				}
			})
		}
		t.Cleanup(restore)
		return restore
	}
}

func newTransferContractRecording(t *testing.T, db *sql.DB, site string, programStartAt time.Time) int64 {
	t.Helper()
	if site == "" {
		site = store.DefaultSite
	}
	if programStartAt.IsZero() {
		programStartAt = time.Now().UTC().Truncate(time.Second)
	}
	return fixtureID(t, execFixture(t, db, `INSERT INTO recordings
(source, site, network_id, service_id, event_id, service_name, channel_type, channel, title, is_free,
 program_start_at, program_duration_ms, status)
VALUES ('manual', ?, 32678, 5168, ?, 'transfer contract', 'GR', '27', 'transfer contract recording', TRUE, ?, 1800000, 'finished')`,
		site, transferContractEventID.Add(1), programStartAt))
}

func transferContractCopy(t *testing.T, db *sql.DB, assetID int64, storeName, relPath, state string, generation int64) int64 {
	t.Helper()
	return fixtureID(t, execFixture(t, db, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation)
VALUES (?, ?, ?, ?, ?)`, assetID, storeName, relPath, state, generation))
}

func waitForTransferLockWaiters(t *testing.T, db *sql.DB, want int, finished <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if finished != nil {
			select {
			case <-finished:
				return
			default:
			}
		}
		var waiting int
		err := db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
FROM performance_schema.data_lock_waits w
JOIN performance_schema.data_locks l
  ON l.ENGINE = w.ENGINE AND l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
WHERE l.OBJECT_SCHEMA = DATABASE()`).Scan(&waiting)
		if err != nil {
			t.Fatalf("observing transfer lock waiters (%d observed, want %d): %v", waiting, want, err)
		}
		if waiting >= want {
			return
		}
		if finished != nil {
			select {
			case <-finished:
				return
			default:
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("only %d transfer operations waited, want %d", waiting, want)
		case <-finished:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestTransferBandwidthLeaseHijacksOnUnlockFailure(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	media := NewMediaInStore(db, "test-store", store.StoragePolicy{})
	lease, acquired, err := media.AcquireTransferSourceBandwidth(t.Context(), "test-store")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("AcquireTransferSourceBandwidth = (%v, %t, %v), want a lease", lease, acquired, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lease.Release(canceled); err == nil {
		t.Fatal("Release with a canceled context succeeded, want the unlock failure")
	}

	// 別の sql.DB を使うことで、観測側が失敗した lease の pool 接続を再利用しないことを保証する。
	var databaseName string
	if err := db.QueryRowContext(t.Context(), `SELECT DATABASE()`).Scan(&databaseName); err != nil {
		t.Fatalf("reading test database name: %v", err)
	}
	cfg, err := mysql.ParseDSN(mysqltest.TestDSN(t))
	if err != nil {
		t.Fatalf("parsing MySQL test DSN: %v", err)
	}
	cfg.DBName = databaseName
	outsideDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("opening observer MySQL pool: %v", err)
	}
	defer func() { _ = outsideDB.Close() }()
	outsideDB.SetMaxOpenConns(1)
	observer, err := outsideDB.Conn(t.Context())
	if err != nil {
		t.Fatalf("borrowing observer connection: %v", err)
	}
	defer func() { _ = observer.Close() }()
	name, err := transferBandwidthLockName(t.Context(), observer, "test-store")
	if err != nil {
		t.Fatalf("building transfer lock name for observer: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var free sql.NullInt64
		if err := observer.QueryRowContext(t.Context(), `SELECT IS_FREE_LOCK(?)`, name).Scan(&free); err != nil {
			t.Fatalf("checking transfer lock from observer connection: %v", err)
		}
		if free.Valid && free.Int64 == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the transfer lock stayed held after failed unlock: the lease connection was returned to the pool")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
