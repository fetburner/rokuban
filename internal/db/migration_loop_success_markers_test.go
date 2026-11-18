package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLoopSuccessMarkersMigrationCopiesEveryRow は 4 つの成功時刻表の行が loop_success_markers に
// 同じ時刻で写り、Down で元の 4 表へ戻ることを見る。写し間違い（loop 名や scope の取り違え）は
// 読み手が次のパスまで 0 を出すので、移行の時点で固定する。
func TestLoopSuccessMarkersMigrationCopiesEveryRow(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	migrateDownTo(t, dbURL, 74) // 4 つの成功時刻表がある版（00074）
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()

	ruler := time.Date(2026, time.November, 1, 1, 0, 0, 0, time.UTC)
	sweep := time.Date(2026, time.November, 1, 2, 0, 0, 0, time.UTC)
	encode := time.Date(2026, time.November, 1, 3, 0, 0, 0, time.UTC)
	deleted := time.Date(2026, time.November, 1, 4, 0, 0, 0, time.UTC)
	// 同じ site 名が ruler と record_sweep の両方にあっても、loop で分かれて残ること。
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ruler_pass_snapshots (site, last_success_at) VALUES ($1, $2)`, []any{"site-a", ruler}},
		{`INSERT INTO record_sweep_snapshots (site, last_success_at) VALUES ($1, $2)`, []any{"site-a", sweep}},
		{`INSERT INTO reconcile_pass_snapshots (pass, last_success_at) VALUES ($1, $2)`, []any{"encode", encode}},
		{`INSERT INTO delete_reconcile_snapshots (store, last_success_at) VALUES ($1, $2)`, []any{"fs-a", deleted}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seeding %s: %v", stmt.sql, err)
		}
	}

	migrateUpTo(t, dbURL, 75)
	want := map[string]time.Time{
		"ruler/site-a":          ruler,
		"record_sweep/site-a":   sweep,
		"reconcile_pass/encode": encode,
		"delete_reconcile/fs-a": deleted,
	}
	if got := readLoopSuccessMarkers(t, pool); !sameMarkers(got, want) {
		t.Fatalf("loop_success_markers after up = %v, want %v", got, want)
	}

	migrateDownTo(t, dbURL, 74)
	checks := []struct {
		table, key, scope string
		want              time.Time
	}{
		{"ruler_pass_snapshots", "site", "site-a", ruler},
		{"record_sweep_snapshots", "site", "site-a", sweep},
		{"reconcile_pass_snapshots", "pass", "encode", encode},
		{"delete_reconcile_snapshots", "store", "fs-a", deleted},
	}
	for _, c := range checks {
		var got time.Time
		query := "SELECT last_success_at FROM " + c.table + " WHERE " + c.key + " = $1"
		if err := pool.QueryRow(ctx, query, c.scope).Scan(&got); err != nil {
			t.Fatalf("reading %s after down: %v", c.table, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("%s(%s) after down = %v, want %v", c.table, c.scope, got, c.want)
		}
	}
}

// readLoopSuccessMarkers は loop_success_markers の全行を "loop/scope" の鍵で返す。
func readLoopSuccessMarkers(t *testing.T, pool *pgxpool.Pool) map[string]time.Time {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT loop, scope, last_success_at FROM loop_success_markers`)
	if err != nil {
		t.Fatalf("querying loop_success_markers: %v", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var loop, scope string
		var at time.Time
		if err := rows.Scan(&loop, &scope, &at); err != nil {
			t.Fatalf("scanning loop_success_markers: %v", err)
		}
		out[loop+"/"+scope] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating loop_success_markers: %v", err)
	}
	return out
}

func sameMarkers(got, want map[string]time.Time) bool {
	if len(got) != len(want) {
		return false
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || !g.Equal(w) {
			return false
		}
	}
	return true
}
