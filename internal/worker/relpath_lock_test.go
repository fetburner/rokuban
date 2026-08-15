package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestIngestRelPathLock_SecondAcquireFailsAndReleaseFrees は
// acquireRelPathLock がセッションレベルの排他になっていることを固定する
// （internal/role/leader_test.go の TestTryAcquire_Exclusive / _ReleaseAndReacquire
// と同じ、独立した 2 プールを使う形）。
//
// **release / pool.Close の登録順序に注意する**（レビュー指摘の教訓を
// このテストにも適用する）。t.Cleanup は LIFO で実行され、かつ t.Fatal は
// その場でテスト関数の goroutine を Goexit するので、通常の `defer` で
// pool.Close を登録していると、release より先（Goexit のスタック巻き戻し中）
// に走ってしまい、保持中のコネクションを待ってハングする。pool.Close を
// 先に t.Cleanup 登録し、release 系を後から t.Cleanup 登録することで、
// どの t.Fatal 経路でも「release が先、Close が後」を保証する。
//
// **どの acquireRelPathLock 呼び出しの戻り値も、結果を見る前に必ず
// t.Cleanup へ登録する。** 「失敗するはず」の呼び出しでも、`acquired` が
// 変異で意図せず true になった場合は本物のコネクションを握ったままになり、
// release を捨てると pool.Close が同じ理由でハングする（このテストを書く
// 過程で実際に踏んだ: pool2 の 2 回目の呼び出しの戻り値を `_` で捨てていたら、
// 壊し方 (SELECT true) を入れたときに pool2.Close がハングした）。
//
// 壊し方: relpath_lock.go の `pg_try_advisory_lock($1)` を `SELECT true` に
// 置き換えると、pool2 が pool1 の保持中でもロックを取得できてしまい、
// 「expected pool2 to NOT acquire」で落ちる（コンパイルは通る変異）。
func TestIngestRelPathLock_SecondAcquireFailsAndReleaseFrees(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	t.Cleanup(pool1.Close)

	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	t.Cleanup(pool2.Close)

	const relPath = "sites/default/test/lock-exclusive.m2ts"

	// safeRelease は release（nil のこともある）を、二重解放しない・nil でも
	// panic しない形にラップする。
	safeRelease := func(release func()) func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				if release != nil {
					release()
				}
			})
		}
	}

	release1, acquired1, err := acquireRelPathLock(ctx, pool1, relPath, time.Second)
	safeRelease1 := safeRelease(release1)
	t.Cleanup(safeRelease1) // pool1.Close より後に登録する（LIFO で先に走る）。
	if err != nil {
		t.Fatalf("acquireRelPathLock pool1: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected pool1 to acquire the lock")
	}

	release2, acquired2, err := acquireRelPathLock(ctx, pool2, relPath, time.Second)
	t.Cleanup(safeRelease(release2)) // pool2.Close より後に登録する。
	if err != nil {
		t.Fatalf("acquireRelPathLock pool2: %v", err)
	}
	if acquired2 {
		t.Fatal("expected pool2 to NOT acquire the lock (already held by pool1)")
	}

	safeRelease1()

	release3, acquired3, err := acquireRelPathLock(ctx, pool2, relPath, time.Second)
	t.Cleanup(safeRelease(release3)) // pool2.Close より後に登録する。
	if err != nil {
		t.Fatalf("acquireRelPathLock pool2 after release: %v", err)
	}
	if !acquired3 {
		t.Fatal("expected pool2 to acquire the lock after pool1 released")
	}
}

// TestIngestWorker_RelPathLockTimeoutDoesNotHang は、プールが枯渇していても
// acquireRelPathLock がハングせず期限内にエラーで返ることを固定する
// （ingest の River タイムアウトは無効なので、これが唯一の歯止め）。
//
// MaxConns=1 のプールの唯一のコネクションを別途保持した状態で
// RelPathLockTimeout=100ms 相当の呼び出しを行う。ジョブ側の相当物である
// acquireRelPathLock の戻りは goroutine + select で 2 秒の期限付きに受け、
// 期限超過はハングではなく t.Fatal（アサーション失敗）で検出する。
//
// 壊し方: acquireRelPathLock 内で pool.Acquire に渡す ctx を
// `acquireCtx`（期限付き）から素の `ctx` に戻すと、2 秒の期限を超えて
// 「did not return within 2s」で落ちる（コンパイルは通る変異）。
func TestIngestWorker_RelPathLockTimeoutDoesNotHang(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parsing pool config: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("creating MaxConns=1 pool: %v", err)
	}
	defer pool.Close()

	// この 1 本きりのコネクションを別途保持し、acquireRelPathLock の
	// pool.Acquire がプール枯渇でハングしうる状況を作る。
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the only connection: %v", err)
	}
	defer held.Release()

	type result struct {
		acquired bool
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		_, acquired, err := acquireRelPathLock(context.Background(), pool, "sites/default/test/lock-timeout.m2ts", 100*time.Millisecond)
		resultCh <- result{acquired: acquired, err: err}
	}()

	select {
	case r := <-resultCh:
		if r.err == nil {
			t.Fatalf("acquireRelPathLock err = nil (acquired=%v), want a timeout error (pool exhausted)", r.acquired)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireRelPathLock did not return within 2s; pool.Acquire hung instead of timing out")
	}
}
