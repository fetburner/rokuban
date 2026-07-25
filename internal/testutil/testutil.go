package testutil

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
)

// DatabaseURL は ROKUBAN_TEST_DATABASE_URL を返す。未設定なら Skip する。
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

// SetupDB はマイグレーションを適用してプールを返す。テスト終了時に Reset + Close する。
//
// テスト用データベースは全パッケージで共有し、各テストが MigrateReset で作り直す。
// go test ./... はパッケージを並行実行するため、advisory lock で DB を使うテストを
// 直列化し、別パッケージのマイグレーションを踏み合わないようにする。
func SetupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := DatabaseURL(t)
	ctx := context.Background()

	// ロック解放（コネクション切断）を最初に登録することで、t.Cleanup の LIFO 順により
	// MigrateReset とプール Close の後に解放される。
	lockTestDB(t, dbURL)

	if err := db.MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("migrate reset: %v", err)
	}
	if err := db.MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		if err := db.MigrateReset(ctx, dbURL); err != nil {
			t.Errorf("cleanup migrate reset: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setLockTimeout はテスト用 DB のロック待ち上限を設定する。
// 別パッケージのテストを待つには十分で、異常時は必ず落ちる値にする。
const setLockTimeout = "SET lock_timeout = '120s'"

// lockHeld はこのプロセスがテスト DB ロックを保持しているかを示す。
var lockHeld atomic.Bool

// lockTestDB はテスト用 DB の排他ロックを取得し、テスト終了時に解放する。
// advisory lock はセッションレベルなので、コネクションを閉じれば解放される。
//
// ロックは毎回別コネクションで取るため再入できない。1 つのテストが SetupDB を 2 回呼ぶと
// 自分自身のロックを待って自己デッドロックするので、同一プロセス内の二重取得は待たずに
// 落とす。プロセスをまたぐ異常は lock_timeout が拾う。
func lockTestDB(t *testing.T, dbURL string) {
	t.Helper()
	ctx := context.Background()

	if !lockHeld.CompareAndSwap(false, true) {
		t.Fatal("test db lock is already held by this process: " +
			"1 つのテストで SetupDB を複数回呼んでいるか、t.Parallel() で並行実行している")
	}
	// 解放フラグを最初に登録することで、t.Cleanup の LIFO 順により
	// 実際のロック解放（コネクション切断）より後に走る。
	t.Cleanup(func() { lockHeld.Store(false) })

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting for test db lock: %v", err)
	}
	if _, err := conn.Exec(ctx, setLockTimeout); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("setting lock_timeout: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", db.TestLockKey); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("acquiring test db lock: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("releasing test db lock: %v", err)
		}
	})
}
