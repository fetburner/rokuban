package mysqlstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
)

// TestCMStationLockRowCreatedConcurrently は、行がまだ無い局の並行する 2 本の lockCMStation を検査する。
// 先の tx が行を作ったまま commit しないとき、後の tx の INSERT は待ち、commit 後に重複として更新に回って成功する。
// ON DUPLICATE KEY UPDATE を外すと後の tx は重複キーで失敗する。
func TestCMStationLockRowCreatedConcurrently(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	station := store.CMStationRef{NetworkID: 32678, ServiceID: 5168}

	tx1, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning first tx: %v", err)
	}
	t.Cleanup(func() { _ = tx1.Rollback() })
	if err := lockCMStation(ctx, tx1, station); err != nil {
		t.Fatalf("first lock of a station without a row: %v", err)
	}

	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning second tx: %v", err)
	}
	t.Cleanup(func() { _ = tx2.Rollback() })
	result := make(chan error, 1)
	go func() { result <- lockCMStation(ctx, tx2, station) }()
	waitLockWaiters(t, db, "named_locks", 1)

	if err := tx1.Commit(); err != nil {
		t.Fatalf("committing first tx: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("second lock of a row created by an uncommitted tx: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock did not return after the first commit")
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("committing second tx: %v", err)
	}

	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM named_locks WHERE name = ?`, cmStationLockName(station)).Scan(&rows); err != nil {
		t.Fatalf("counting station lock rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("station lock rows = %d, want 1", rows)
	}
}
