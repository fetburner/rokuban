package db

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/config"
)

// lockHeld はこのプロセスがテスト DB ロックを保持しているかを示す。
var lockHeld atomic.Bool

// testDatabaseURL は ROKUBAN_TEST_DATABASE_URL を返し、テスト用 DB の排他ロックを取る。
// 未設定なら Skip する。呼び出し元はいずれもマイグレーションを張り替えるため、
// URL の取得と同時にロックする（testutil.SetupDB と同じ規律。db は testutil を
// 参照できないためロック処理をここに持つ）。
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	lockTestDB(t, url)
	return url
}

// lockTestDB はテスト用 DB の排他ロックを取得し、テスト終了時に解放する。
// advisory lock はセッションレベルなので、コネクションを閉じれば解放される。
// t.Cleanup は LIFO なので、最初に登録することでマイグレーション後始末の後に解放される。
//
// ロックは毎回別コネクションで取るため再入できない。1 つのテストが testDatabaseURL を
// 2 回呼ぶと自分自身のロックを待って自己デッドロックするので、同一プロセス内の二重取得は
// 待たずに落とす。プロセスをまたぐ異常は lock_timeout が拾う。
func lockTestDB(t *testing.T, dbURL string) {
	t.Helper()
	ctx := context.Background()

	if !lockHeld.CompareAndSwap(false, true) {
		t.Fatal("test db lock is already held by this process: " +
			"1 つのテストで testDatabaseURL を複数回呼んでいるか、t.Parallel() で並行実行している")
	}
	t.Cleanup(func() { lockHeld.Store(false) })

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting for test db lock: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET lock_timeout = '120s'"); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("setting lock_timeout: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", TestLockKey); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("acquiring test db lock: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("releasing test db lock: %v", err)
		}
	})
}

func TestMigrateUpDown(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
}

func TestNewPool(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		_ = MigrateDown(ctx, dbURL)
	})

	cfg := config.DBConfig{
		Host:     "localhost",
		Port:     5432,
		User:     "rokuban",
		Password: "rokuban",
		Database: "rokuban_test",
		SSLMode:  "disable",
	}

	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var value string
	err = pool.QueryRow(ctx, "SELECT value FROM schema_info WHERE key = 'version'").Scan(&value)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if value != "1" {
		t.Errorf("schema_info version = %q, want %q", value, "1")
	}
}

func TestNewPool_ConnectionFailure(t *testing.T) {
	cfg := config.DBConfig{
		Host:     "localhost",
		Port:     59999,
		User:     "nonexistent",
		Password: "nonexistent",
		Database: "nonexistent",
		SSLMode:  "disable",
	}

	ctx := context.Background()
	_, err := NewPool(ctx, cfg)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}
