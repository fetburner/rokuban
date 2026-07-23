package role

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

var fastConfig = &SingletonConfig{
	PollInterval:      100 * time.Millisecond,
	HeartbeatInterval: 100 * time.Millisecond,
}

func TestTryAcquire_Exclusive(t *testing.T) {
	dbURL := testDatabaseURL(t)
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

	acquired1, release1, err := TryAcquire(ctx, pool1, "ruler")
	if err != nil {
		t.Fatalf("TryAcquire on pool1: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected pool1 to acquire lock")
	}
	defer release1()

	acquired2, _, err := TryAcquire(ctx, pool2, "ruler")
	if err != nil {
		t.Fatalf("TryAcquire on pool2: %v", err)
	}
	if acquired2 {
		t.Fatal("expected pool2 to NOT acquire lock (already held by pool1)")
	}
}

func TestTryAcquire_ReleaseAndReacquire(t *testing.T) {
	dbURL := testDatabaseURL(t)
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

	acquired, release, err := TryAcquire(ctx, pool1, "reconciler")
	if err != nil {
		t.Fatalf("TryAcquire on pool1: %v", err)
	}
	if !acquired {
		t.Fatal("expected pool1 to acquire lock")
	}

	release()

	acquired2, release2, err := TryAcquire(ctx, pool2, "reconciler")
	if err != nil {
		t.Fatalf("TryAcquire on pool2 after release: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected pool2 to acquire lock after pool1 released")
	}
	defer release2()
}

func TestTryAcquire_DifferentRoles(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	defer pool.Close()

	acquired1, release1, err := TryAcquire(ctx, pool, "ruler")
	if err != nil {
		t.Fatalf("TryAcquire ruler: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected to acquire ruler lock")
	}
	defer release1()

	acquired2, release2, err := TryAcquire(ctx, pool, "reconciler")
	if err != nil {
		t.Fatalf("TryAcquire reconciler: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected to acquire reconciler lock (different role)")
	}
	defer release2()
}

func TestRunSingleton_ContextCancel(t *testing.T) {
	dbURL := testDatabaseURL(t)

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
		_ = RunSingleton(ctx, pool, "test-cancel", func(ctx context.Context) error {
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
	dbURL := testDatabaseURL(t)

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
		_ = RunSingleton(leader1Ctx, pool1, "test-failover", func(ctx context.Context) error {
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
		_ = RunSingleton(ctx, pool2, "test-failover", func(ctx context.Context) error {
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
	dbURL := testDatabaseURL(t)

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
		_ = RunSingleton(ctx, pool, "test-restart", func(ctx context.Context) error {
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
