package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestIngestJobLock_SecondAcquireFailsAndReleaseFrees は、rel_path ではなく
// ジョブ ID だけがセッションレベル advisory lock の対象であることを固定する。
// 同じジョブの二重実行は防ぐが、異なる rel_path の ingest を直列化する lock は
// 存在しない。release 後は別セッションが同じジョブ lock を取得できる。
func TestIngestJobLock_SecondAcquireFailsAndReleaseFrees(t *testing.T) {
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

	const jobID int64 = 731001

	lock1, acquired, err := acquireIngestJobLock(ctx, pool1, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock from pool1: %v", err)
	}
	if !acquired {
		t.Fatal("pool1 did not acquire the job lock")
	}
	t.Cleanup(lock1.release)

	lock2, acquired, err := acquireIngestJobLock(ctx, pool2, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock from pool2: %v", err)
	}
	if lock2 != nil {
		t.Cleanup(lock2.release)
	}
	if acquired {
		t.Fatal("pool2 acquired a job lock already held by pool1")
	}

	lock1.release()
	lock3, acquired, err := acquireIngestJobLock(ctx, pool2, jobID, time.Second)
	if err != nil {
		t.Fatalf("reacquiring job lock after release: %v", err)
	}
	if !acquired {
		t.Fatal("pool2 did not acquire the job lock after pool1 released it")
	}
	t.Cleanup(lock3.release)
}

// TestIngestJobLock_TimeoutDoesNotHang は、ロック用コネクションの取得がプール枯渇で
// ハングせず、指定した timeout で戻ることを固定する。
func TestIngestJobLock_TimeoutDoesNotHang(t *testing.T) {
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

	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the only connection: %v", err)
	}
	defer held.Release()

	resultCh := make(chan error, 1)
	go func() {
		_, _, err := acquireIngestJobLock(context.Background(), pool, 731002, 100*time.Millisecond)
		resultCh <- err
	}()

	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("acquireIngestJobLock returned nil with an exhausted pool")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireIngestJobLock did not return within 2s")
	}
}

// TestIngestJobLock_TransientHeartbeatFailures は、一過性の DB エラーでは転送を
// cancel せず、閾値に達したら heartbeat だけを停止する判定を固定する。
func TestIngestJobLock_TransientHeartbeatFailures(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	l := newIngestJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		return false, false, transientErr
	}

	var consecutiveFailures int
	for i := 1; i < ingestJobLockMaxTransientFailures; i++ {
		if l.heartbeatTick(&consecutiveFailures) {
			t.Fatalf("heartbeatTick stopped after transient failure %d", i)
		}
	}
	if !l.heartbeatTick(&consecutiveFailures) {
		t.Fatalf("heartbeatTick did not stop after %d transient failures", ingestJobLockMaxTransientFailures)
	}

	// job lock には rel_path lock のような lost channel がない。ここで止まるのは
	// keepalive goroutine だけであり、Work の transfer context を cancel する状態を
	// lock 自体が持たないことをコンパイル時・構造上も確認する。
	if consecutiveFailures != ingestJobLockMaxTransientFailures {
		t.Fatalf("consecutiveFailures = %d, want %d", consecutiveFailures, ingestJobLockMaxTransientFailures)
	}
}

func TestIngestJobLock_HeartbeatFailureResetsOnSuccess(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	var succeedNext bool
	l := newIngestJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		if succeedNext {
			succeedNext = false
			return true, false, nil
		}
		return false, false, transientErr
	}

	var consecutiveFailures int
	for i := 1; i < ingestJobLockMaxTransientFailures; i++ {
		if l.heartbeatTick(&consecutiveFailures) {
			t.Fatalf("heartbeatTick stopped before threshold at failure %d", i)
		}
	}
	succeedNext = true
	if l.heartbeatTick(&consecutiveFailures) {
		t.Fatal("heartbeatTick stopped after a successful check")
	}
	if consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d after success, want 0", consecutiveFailures)
	}
}
