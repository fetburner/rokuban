package pglocker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/testutil"
)

var fastConfig = &role.SingletonConfig{
	PollInterval:      100 * time.Millisecond,
	HeartbeatInterval: 100 * time.Millisecond,
}

func TestTryAcquire_Exclusive(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	defer pool1.Close()

	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	defer pool2.Close()

	sess1, err := New(pool1).Checkout(ctx)
	if err != nil {
		t.Fatalf("Checkout on pool1: %v", err)
	}
	defer sess1.Release()
	acquired1, err := sess1.TryLock(ctx, "ruler")
	if err != nil {
		t.Fatalf("TryLock on pool1: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected pool1 to acquire lock")
	}
	defer func() { _ = sess1.Unlock(ctx, "ruler") }()

	sess2, err := New(pool2).Checkout(ctx)
	if err != nil {
		t.Fatalf("Checkout on pool2: %v", err)
	}
	defer sess2.Release()
	acquired2, err := sess2.TryLock(ctx, "ruler")
	if err != nil {
		t.Fatalf("TryLock on pool2: %v", err)
	}
	if acquired2 {
		t.Fatal("expected pool2 to NOT acquire lock (already held by pool1)")
	}
}

func TestTryAcquire_ReleaseAndReacquire(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	defer pool1.Close()

	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	defer pool2.Close()

	sess1, err := New(pool1).Checkout(ctx)
	if err != nil {
		t.Fatalf("Checkout on pool1: %v", err)
	}
	acquired, err := sess1.TryLock(ctx, "reconciler")
	if err != nil {
		t.Fatalf("TryLock on pool1: %v", err)
	}
	if !acquired {
		t.Fatal("expected pool1 to acquire lock")
	}

	if err := sess1.Unlock(ctx, "reconciler"); err != nil {
		t.Fatalf("Unlock on pool1: %v", err)
	}
	sess1.Release()

	sess2, err := New(pool2).Checkout(ctx)
	if err != nil {
		t.Fatalf("Checkout on pool2: %v", err)
	}
	defer sess2.Release()
	acquired2, err := sess2.TryLock(ctx, "reconciler")
	if err != nil {
		t.Fatalf("TryLock on pool2 after release: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected pool2 to acquire lock after pool1 released")
	}
	defer func() { _ = sess2.Unlock(ctx, "reconciler") }()
}

func TestTryAcquire_DifferentRoles(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	defer pool.Close()

	sess1, err := New(pool).Checkout(ctx)
	if err != nil {
		t.Fatalf("Checkout ruler: %v", err)
	}
	defer sess1.Release()
	acquired1, err := sess1.TryLock(ctx, "ruler")
	if err != nil {
		t.Fatalf("TryLock ruler: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected to acquire ruler lock")
	}
	defer func() { _ = sess1.Unlock(ctx, "ruler") }()

	sess2, err := New(pool).Checkout(ctx)
	if err != nil {
		t.Fatalf("Checkout reconciler: %v", err)
	}
	defer sess2.Release()
	acquired2, err := sess2.TryLock(ctx, "reconciler")
	if err != nil {
		t.Fatalf("TryLock reconciler: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected to acquire reconciler lock (different role)")
	}
	defer func() { _ = sess2.Unlock(ctx, "reconciler") }()
}

func TestRunSingleton_ContextCancel(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var started atomic.Bool
	roleStarted := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = role.RunSingletonWith(ctx, New(pool), "test-cancel", func(ctx context.Context) error {
			started.Store(true)
			close(roleStarted)
			<-ctx.Done()
			return ctx.Err()
		}, fastConfig)
	}()

	select {
	case <-roleStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for role to start")
	}

	if !started.Load() {
		t.Fatal("role was not started")
	}

	cancel()
	wg.Wait()
}

func TestRunSingleton_Failover(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)

	pool1, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	defer pool1.Close()

	pool2, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	defer pool2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// プロセス 1 がロックを取得
	leader1Started := make(chan struct{})
	leader1Ctx, leader1Cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = role.RunSingletonWith(leader1Ctx, New(pool1), "test-failover", func(ctx context.Context) error {
			close(leader1Started)
			<-ctx.Done()
			return ctx.Err()
		}, fastConfig)
	}()

	select {
	case <-leader1Started:
	case <-ctx.Done():
		t.Fatal("timed out waiting for leader1 to start")
	}

	// プロセス 2 は待機中（ロック取得できない）
	leader2Started := make(chan struct{}, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = role.RunSingletonWith(ctx, New(pool2), "test-failover", func(ctx context.Context) error {
			select {
			case leader2Started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		}, fastConfig)
	}()

	// 少し待って、プロセス 2 がまだ起動していないことを確認
	time.Sleep(300 * time.Millisecond)
	select {
	case <-leader2Started:
		t.Fatal("leader2 should not have started while leader1 holds the lock")
	default:
	}

	// プロセス 1 を停止 → プロセス 2 がフェイルオーバーでロック取得
	leader1Cancel()

	select {
	case <-leader2Started:
		// フェイルオーバー成功
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for leader2 to acquire lock after leader1 stopped")
	}

	cancel()
	wg.Wait()
}

func TestRunSingleton_RoleRestart(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// roleFunc がエラーで返ったら、監督ループが再起動する
	var runCount atomic.Int32
	secondRun := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = role.RunSingletonWith(ctx, New(pool), "test-restart", func(ctx context.Context) error {
			n := runCount.Add(1)
			if n == 1 {
				return fmt.Errorf("simulated crash")
			}
			close(secondRun)
			<-ctx.Done()
			return ctx.Err()
		}, fastConfig)
	}()

	select {
	case <-secondRun:
		// ロールが再起動された
	case <-ctx.Done():
		t.Fatal("timed out waiting for role to restart after crash")
	}

	if runCount.Load() < 2 {
		t.Errorf("expected at least 2 runs, got %d", runCount.Load())
	}

	cancel()
	wg.Wait()
}

// TestPgLockKey_Literal は PostgreSQL の鍵が固定の値であることを確かめる。鍵はプロセス間で共有されるので、
// 値が変わると新旧のプロセスが別の鍵を取る。期待値はリテラルで書く（実装の関数から作らない）。
func TestPgLockKey_Literal(t *testing.T) {
	if got := pgLockKey("ruler"); got != -7623009511659812475 {
		t.Errorf("pgLockKey(ruler) = %d, want -7623009511659812475", got)
	}
	if got := pgLockKey("watcher:tokyo"); got != -7779555942240376231 {
		t.Errorf("pgLockKey(watcher:tokyo) = %d, want -7779555942240376231", got)
	}
}
