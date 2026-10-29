package pgstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestAdoptUnassignedStoreRowsAssignsLegacyDeleteReconcileBreaker は、列の改名前に
// 空の範囲で書かれた delete_reconcile ラッチを storage adopt が置き場へ割り当てることを確認する。
func TestAdoptUnassignedStoreRowsAssignsLegacyDeleteReconcileBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, "tokyo", "")
	trippedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (scope, name, tripped_at, pending, threshold, detail) VALUES
  ('', 'delete_reconcile', $1, 150, 100, '{"total":150}'::jsonb),
  ('default', 'ruler_deletes', $1, 7, 5, '{"total":7}'::jsonb),
  ('', 'ruler_deletes', $1, 9, 5, '{"total":9}'::jsonb)`, trippedAt); err != nil {
		t.Fatalf("seeding circuit breakers: %v", err)
	}

	// 空の範囲の行が残っている間は起動を拒む。site の範囲の行は数えない。
	err := media.RequireAdoptedStoreRows(ctx)
	if err == nil || !strings.Contains(err.Error(), "1 media store row(s)") {
		t.Fatalf("RequireAdoptedStoreRows with a legacy breaker = %v, want an error counting 1 row", err)
	}
	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err != nil {
		t.Fatalf("RequireAdoptedStoreRows after adoption: %v", err)
	}

	// 割り当て後の行は発動時刻・件数・閾値・detail を保つ。空の範囲の行は残らない。
	var pending, threshold int
	var gotTrippedAt time.Time
	var detail string
	if err := pool.QueryRow(ctx, `SELECT tripped_at, pending, threshold, detail::text FROM circuit_breakers WHERE scope = 'tokyo' AND name = 'delete_reconcile'`).
		Scan(&gotTrippedAt, &pending, &threshold, &detail); err != nil {
		t.Fatalf("reading adopted breaker: %v", err)
	}
	if !gotTrippedAt.Equal(trippedAt) || pending != 150 || threshold != 100 || detail != `{"total": 150}` {
		t.Errorf("adopted breaker = (%v, %d, %d, %s), want (%v, 150, 100, {\"total\": 150})", gotTrippedAt, pending, threshold, detail, trippedAt)
	}
	if got := countBreakerRows(t, pool, `scope = '' AND name = 'delete_reconcile'`); got != 0 {
		t.Errorf("empty-scope delete_reconcile rows = %d, want 0", got)
	}

	// delete_reconcile 以外の行は動かない。範囲が空の ruler_deletes も、
	// リテラル 'delete_reconcile' で絞っているので割り当てられない。
	if got := countBreakerRows(t, pool, `scope = 'default' AND name = 'ruler_deletes' AND pending = 7`); got != 1 {
		t.Errorf("site-scoped ruler_deletes rows = %d, want 1 untouched", got)
	}
	if got := countBreakerRows(t, pool, `scope = '' AND name = 'ruler_deletes' AND pending = 9`); got != 1 {
		t.Errorf("empty-scope ruler_deletes rows = %d, want 1 untouched", got)
	}
	if got := countBreakerRows(t, pool, `scope = 'tokyo' AND name = 'ruler_deletes'`); got != 0 {
		t.Errorf("tokyo ruler_deletes rows = %d, want 0", got)
	}
}

// TestAdoptUnassignedStoreRowsKeepsExistingStoreBreaker は、置き場の範囲の行が既にあっても
// adopt が成功し、行が 1 つだけ残ることを確認する。置き場側の行を正とする。
func TestAdoptUnassignedStoreRowsKeepsExistingStoreBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, "tokyo", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (scope, name, pending, threshold, detail) VALUES
  ('', 'delete_reconcile', 150, 100, '{"total":150}'::jsonb),
  ('tokyo', 'delete_reconcile', 42, 100, '{"total":42}'::jsonb)`); err != nil {
		t.Fatalf("seeding circuit breakers: %v", err)
	}
	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows: %v", err)
	}
	if got := countBreakerRows(t, pool, `name = 'delete_reconcile'`); got != 1 {
		t.Errorf("delete_reconcile rows = %d, want 1", got)
	}
	if got := countBreakerRows(t, pool, `scope = 'tokyo' AND name = 'delete_reconcile' AND pending = 42`); got != 1 {
		t.Errorf("tokyo delete_reconcile rows with pending 42 = %d, want 1", got)
	}
}

func countBreakerRows(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM circuit_breakers WHERE `+where).Scan(&n); err != nil {
		t.Fatalf("counting circuit breakers where %s: %v", where, err)
	}
	return n
}
