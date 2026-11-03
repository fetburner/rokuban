package testutil

import (
	"context"
	"testing"
	"time"
)

// handshakeTimeout は並列サブテスト同士の待ち合わせの上限。共有 DB を返す壊れた実装では
// 相手の書き込みが見えて検査が落ちるので、待ちが切れるのは正常系が崩れたときだけになる。
const handshakeTimeout = 10 * time.Second

// TestSetupDB_ParallelTestsDoNotShareRows は、並列に走る 2 つのテストが別々の DB を
// 受け取ることを確かめる。writer が書いた行を reader が見てしまう（SetupDB が 1 つの DB を
// 共有している）なら、reader の件数検査が落ちる。
func TestSetupDB_ParallelTestsDoNotShareRows(t *testing.T) {
	written := make(chan struct{})
	checked := make(chan struct{})

	t.Run("writer", func(t *testing.T) {
		t.Parallel()
		pool := SetupDB(t)
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes) VALUES ('media', 'writer', 1, 0, 0)`); err != nil {
			t.Fatalf("inserting marker: %v", err)
		}
		close(written)
		select {
		case <-checked:
		case <-time.After(handshakeTimeout):
			t.Fatal("reader did not finish its check in time")
		}
	})

	t.Run("reader", func(t *testing.T) {
		t.Parallel()
		defer close(checked)
		pool := SetupDB(t)
		select {
		case <-written:
		case <-time.After(handshakeTimeout):
			t.Fatal("writer did not write in time")
		}
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM storage_sync WHERE path = 'writer'`).Scan(&n); err != nil {
			t.Fatalf("counting markers: %v", err)
		}
		if n != 0 {
			t.Fatalf("reader sees %d rows written by another test; want 0 (databases must not be shared)", n)
		}
	})
}

// TestSetupDB_StartsEmptyFromMigratedTemplate は、新しい DB がマイグレーション済みで、
// 行を含まないことを確かめる。テンプレートはマイグレーション後に TRUNCATE で空にしている。
func TestSetupDB_StartsEmptyFromMigratedTemplate(t *testing.T) {
	t.Parallel()
	pool := SetupDB(t)
	ctx := context.Background()

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.recordings') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("checking recordings: %v", err)
	}
	if !exists {
		t.Fatal("recordings is missing; the template must be migrated")
	}

	for _, table := range []string{"recordings", "storage_sync", "river_job"} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("counting %s rows: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows; want 0 (the template is cleared after migration)", table, n)
		}
	}
}

// TestSetupDB_SecondCallStartsEmpty は、同じテストで 2 回目の SetupDB が DB を空にしてから
// 返すことを確かめる（呼ぶたびに空の DB、という契約）。2 回目を空にしない実装では落ちる。
func TestSetupDB_SecondCallStartsEmpty(t *testing.T) {
	t.Parallel()
	first := SetupDB(t)
	ctx := context.Background()
	if _, err := first.Exec(ctx, `INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes) VALUES ('media', 'first', 1, 0, 0)`); err != nil {
		t.Fatalf("inserting row: %v", err)
	}

	second := SetupDB(t)
	var n int
	if err := second.QueryRow(ctx, `SELECT count(*) FROM storage_sync`).Scan(&n); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("second SetupDB sees %d rows; want 0", n)
	}
}

// TestDatabaseConfig_PointsAtSetupDBDatabase は、DatabaseConfig を SetupDB より前に呼んでも
// 後に呼んでも、同じテストの DB を指すことを確かめる。
func TestDatabaseConfig_PointsAtSetupDBDatabase(t *testing.T) {
	t.Parallel()
	before := DatabaseConfig(t)
	pool := SetupDB(t)
	after := DatabaseConfig(t)

	var name string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if before.Database != name || after.Database != name {
		t.Fatalf("config databases = %q (before SetupDB) and %q (after); pool database = %q", before.Database, after.Database, name)
	}
}
