package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TerminateOpenTransactions は、トランザクションを開いたまま待っている他の backend
// （idle in transaction）を pool の別接続から落とし、消えるまで待つ。
// store の publish 関数の中から呼ぶと、publish が成功した後の commit だけが失敗する状況を作れる。
func TerminateOpenTransactions(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'idle in transaction' AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Fatalf("listing open transactions: %v", err)
	}
	var pids []int32
	for rows.Next() {
		var pid int32
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			t.Fatalf("scanning open transaction pid: %v", err)
		}
		pids = append(pids, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("listing open transactions: %v", err)
	}
	if len(pids) == 0 {
		t.Fatal("no open transaction to terminate; the caller must run inside a store transaction")
	}
	for _, pid := range pids {
		if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("terminating backend %d: %v", pid, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var remaining int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1)`, pids).Scan(&remaining); err != nil {
			t.Fatalf("waiting for terminated backends: %v", err)
		}
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends still alive after pg_terminate_backend", remaining)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
