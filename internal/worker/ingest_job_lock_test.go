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

// TestIngestJobLock_TransientHeartbeatFailuresNeverStop は、一過性の DB エラーでは
// heartbeat が止まらないことを固定する。旧設計（rel_path advisory lock）では
// 「heartbeat 停止 = markLost = 転送キャンセル」という終端判断だったので閾値で
// 止める理由があったが、新設計の heartbeat の唯一の仕事は job lock 用セッションを
// idle 切断から守る keepalive である（型の doc コメント参照）。一過性失敗で
// keepalive 自身を止めると、唯一の保護を自分から捨てることになる
// （セッションが idle のまま放置 → pgbouncer 等の idle timeout で切断 →
// advisory lock 解放 → record_sweep が生存中の running 行を discard → 重複
// ジョブ投入 → 全量再ダウンロード）。
func TestIngestJobLock_TransientHeartbeatFailuresNeverStop(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	l := newIngestJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		return false, false, transientErr
	}

	for i := 1; i <= 50; i++ {
		if l.heartbeatTick() {
			t.Fatalf("heartbeatTick stopped after transient failure %d; transient failures must never stop the keepalive", i)
		}
	}
}

// TestIngestJobLock_PermanentHeartbeatFailureStops は、コネクション切断
// （permanent）を検知したら即座に heartbeat を止めることを固定する。
func TestIngestJobLock_PermanentHeartbeatFailureStops(t *testing.T) {
	permanentErr := errors.New("simulated closed connection")
	l := newIngestJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		return false, true, permanentErr
	}

	if !l.heartbeatTick() {
		t.Fatal("heartbeatTick did not stop on a permanent connection failure")
	}
}

// TestIngestJobLock_LockLostStopsHeartbeat は、lock 喪失が確定した
// （held=false, err=nil）場合に heartbeat を止めることを固定する。
func TestIngestJobLock_LockLostStopsHeartbeat(t *testing.T) {
	l := newIngestJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		return false, false, nil
	}

	if !l.heartbeatTick() {
		t.Fatal("heartbeatTick did not stop after the lock was confirmed lost")
	}
}

// TestIngestJobLock_HeartbeatRecoversAfterTransientFailures は、一過性失敗が
// 何度続いても、その後 held=true が返れば heartbeat が動き続けることを固定する。
func TestIngestJobLock_HeartbeatRecoversAfterTransientFailures(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	var succeedNext bool
	l := newIngestJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		if succeedNext {
			return true, false, nil
		}
		return false, false, transientErr
	}

	for i := 1; i <= 5; i++ {
		if l.heartbeatTick() {
			t.Fatalf("heartbeatTick stopped before recovery at failure %d", i)
		}
	}
	succeedNext = true
	if l.heartbeatTick() {
		t.Fatal("heartbeatTick stopped after a successful check")
	}
}
