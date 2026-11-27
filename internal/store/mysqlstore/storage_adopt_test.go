package mysqlstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
)

// TestAdoptUnassignedStoreRowsAssignsLegacyDeleteReconcileBreaker は、置き場に割り当てられていない
// delete_reconcile ブレーカーがこの置き場へ移り、残りが無くなることを確かめる。
// 実 MySQL の storage adopt 実行で見つかった、INSERT ... SELECT の列名の曖昧さを防ぐための検査でもある。
func TestAdoptUnassignedStoreRowsAssignsLegacyDeleteReconcileBreaker(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO circuit_breakers (scope, name, pending, threshold, detail) VALUES ('', 'delete_reconcile', 0, 1, JSON_OBJECT())`); err != nil {
		t.Fatalf("inserting legacy breaker: %v", err)
	}
	media := NewMediaInStore(db, "main", store.StoragePolicy{})

	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows: %v", err)
	}
	assertBreakerCount(t, db, "main", 1)
	assertBreakerCount(t, db, "", 0)

	// 既に割り当て済みの置き場がある状態で再実行しても、割り当て先の行は消えず、失敗もしない。
	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("second AdoptUnassignedStoreRows: %v", err)
	}
	assertBreakerCount(t, db, "main", 1)
}

// TestHasStoreCopiesReportsExistingCopies は、この置き場のコピー行の有無を返すことを確かめる。
func TestHasStoreCopiesReportsExistingCopies(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	media := NewMediaInStore(db, "main", store.StoragePolicy{})

	has, err := media.HasStoreCopies(ctx)
	if err != nil {
		t.Fatalf("HasStoreCopies: %v", err)
	}
	if has {
		t.Fatal("HasStoreCopies = true for an empty database, want false")
	}
}

// TestRequireAdoptedStoreRowsRejectsUnassignedBreaker は、置き場の割り当て前の行が残っている間は
// 起動の検査が失敗し、割り当てた後は通ることを確かめる（worker の起動を止める guard）。
func TestRequireAdoptedStoreRowsRejectsUnassignedBreaker(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	media := NewMediaUnboundWithPolicy(db, store.StoragePolicy{})
	if _, err := db.ExecContext(ctx, `INSERT INTO circuit_breakers (scope, name, pending, threshold, detail) VALUES ('', 'delete_reconcile', 0, 1, JSON_OBJECT())`); err != nil {
		t.Fatalf("inserting unassigned breaker: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err == nil {
		t.Fatal("RequireAdoptedStoreRows = nil with an unassigned breaker, want an error")
	}
	if err := NewMediaInStore(db, "main", store.StoragePolicy{}).AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err != nil {
		t.Fatalf("RequireAdoptedStoreRows after adoption: %v", err)
	}
}

func assertBreakerCount(t *testing.T, db *sql.DB, scope string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM circuit_breakers WHERE scope = ? AND name = 'delete_reconcile'`, scope).Scan(&got); err != nil {
		t.Fatalf("counting breakers for scope %q: %v", scope, err)
	}
	if got != want {
		t.Fatalf("delete_reconcile breakers with scope %q = %d, want %d", scope, got, want)
	}
}
