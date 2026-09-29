package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// useJobLockTimings は heartbeat 間隔と idle_session_timeout をテスト用の短い値に
// 差し替える。本番の 1 秒 / 30 秒と同じ順序（間隔 ≪ timeout）を保ったまま、実時間の
// 待ちを秒未満に縮める。並列実行はしない（パッケージ変数を書き換えるため）。
func useJobLockTimings(t *testing.T, interval, idleTimeout time.Duration) {
	t.Helper()
	previousInterval, previousIdleTimeout := jobLockHeartbeatInterval, jobLockIdleSessionTimeout
	jobLockHeartbeatInterval, jobLockIdleSessionTimeout = interval, idleTimeout
	t.Cleanup(func() {
		jobLockHeartbeatInterval, jobLockIdleSessionTimeout = previousInterval, previousIdleTimeout
	})
}

// newTestPool は testutil.DatabaseURL の DB に対する独立したプールを作る。
// advisory lock の持ち主と回収側を別セッションにするために 2 本使う。
func newTestPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestIngestJobLock_IdleSessionTimeoutReleasesLockAfterHeartbeatStops は、heartbeat が
// 止まった lock 用セッションを Postgres が idle_session_timeout で終了させ、advisory
// lock が解放されることを固定する。コネクションは開いたままで、クライアントが黙った
// だけという状態（SIGSTOP / ノード消失 / ネットワーク分断の等価物）を作る。
//
// これが無いと、回収の 2 段目（recoverStaleIngestJobs の pg_try_advisory_lock）が
// !acquired に落ち続け、running 行が誰にも回収されないまま滞留する。
func TestIngestJobLock_IdleSessionTimeoutReleasesLockAfterHeartbeatStops(t *testing.T) {
	useJobLockTimings(t, 100*time.Millisecond, time.Second)

	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()
	pool1 := newTestPool(t, dbURL)
	pool2 := newTestPool(t, dbURL)

	const jobID int64 = 731003
	lock1, acquired, err := acquireIngestJobLock(ctx, pool1, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock from pool1: %v", err)
	}
	if !acquired {
		t.Fatal("pool1 did not acquire the job lock")
	}
	t.Cleanup(lock1.release)

	var backendPID int
	if err := lock1.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPID); err != nil {
		t.Fatalf("reading lock backend pid: %v", err)
	}

	// heartbeat を止める。ノード死・SIGSTOP・分断ではこれが起きる。コネクションは
	// 開いたままにする（TCP keepalive はカーネルが撃つだけで、バックエンドの
	// idle タイマーは更新しない）。
	lock1.stopHeartbeatLoop()
	stoppedAt := time.Now()

	// 別セッションが lock を取れるようになるまでの実時間を測る。取れなければ
	// deadline で落ちる（壊れたときにハングさせない）。
	deadline := stoppedAt.Add(5 * time.Second)
	var elapsed time.Duration
	for {
		lock2, acquired, err := acquireIngestJobLock(ctx, pool2, jobID, time.Second)
		if err != nil {
			t.Fatalf("acquiring job lock after the session was terminated: %v", err)
		}
		if acquired {
			lock2.release()
			elapsed = time.Since(stoppedAt)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat 停止から 5 秒経っても別セッションが lock を取得できない（idle_session_timeout が効いていない）")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("idle_session_timeout=1s: heartbeat 停止から %v で別セッションが lock を取得した", elapsed.Round(time.Millisecond))

	var backendAlive bool
	if err := pool1.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", backendPID,
	).Scan(&backendAlive); err != nil {
		t.Fatalf("checking lock backend liveness: %v", err)
	}
	if backendAlive {
		t.Fatalf("lock backend pid %d is still in pg_stat_activity after the lock was released", backendPID)
	}
}

// TestIngestJobLock_HeartbeatKeepsSessionAlive は、heartbeat が動いている間は
// idle_session_timeout が経過してもセッションが終了しないことを固定する。
// timeout の 3 倍の間、別セッションは一度も lock を取得できない。heartbeat を
// 止めると同じテストが落ちる（lease の両方向）。
func TestIngestJobLock_HeartbeatKeepsSessionAlive(t *testing.T) {
	useJobLockTimings(t, 100*time.Millisecond, time.Second)

	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()
	pool1 := newTestPool(t, dbURL)
	pool2 := newTestPool(t, dbURL)

	const jobID int64 = 731004
	lock1, acquired, err := acquireIngestJobLock(ctx, pool1, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock from pool1: %v", err)
	}
	if !acquired {
		t.Fatal("pool1 did not acquire the job lock")
	}
	t.Cleanup(lock1.release)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		lock2, acquired, err := acquireIngestJobLock(ctx, pool2, jobID, time.Second)
		if err != nil {
			t.Fatalf("acquiring job lock from pool2 during a live heartbeat: %v", err)
		}
		if acquired {
			lock2.release()
			t.Fatal("heartbeat が動いているのに別セッションが lock を取得した（idle_session_timeout が生きたセッションを終了させた）")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// idleSessionTimeoutOnPooledConn はプールが保持している（= 次の Acquire が返す）
// コネクションの idle_session_timeout を SHOW で読む。
func idleSessionTimeoutOnPooledConn(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a pooled connection: %v", err)
	}
	defer conn.Release()
	var value string
	if err := conn.QueryRow(ctx, "SHOW idle_session_timeout").Scan(&value); err != nil {
		t.Fatalf("reading idle_session_timeout of a pooled connection: %v", err)
	}
	return value
}

// TestIngestJobLock_SessionTimeoutNeverLeaksToAPooledConnection は、lease の設定が
// プールへ返ったコネクションに残らないことを固定する。残すと、そのコネクションを
// 後で掴んだ無関係のクエリが、idle のまま Postgres に切られたソケットへ書き込む。
// 2 経路を見る: (1) lock を取れなかった経路（pg_try_advisory_lock と SET を同じ
// 往復にすると、ここで設定が付いたままプールに戻る）、(2) release した経路。
func TestIngestJobLock_SessionTimeoutNeverLeaksToAPooledConnection(t *testing.T) {
	useJobLockTimings(t, 100*time.Millisecond, time.Second)

	ctx := context.Background()
	holder := newTestPool(t, testutil.DatabaseURL(t))

	cfg, err := pgxpool.ParseConfig(testutil.DatabaseURL(t))
	if err != nil {
		t.Fatalf("parsing pool config: %v", err)
	}
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("creating MaxConns=1 pool: %v", err)
	}
	t.Cleanup(single.Close)

	const jobID int64 = 731005
	lock, acquired, err := acquireIngestJobLock(ctx, holder, jobID, time.Second)
	if err != nil || !acquired {
		t.Fatalf("acquiring the job lock from the holder pool: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(lock.release)

	// (1) 取得に失敗した経路。このコネクションはプールへ戻る。
	if _, acquired, err := acquireIngestJobLock(ctx, single, jobID, time.Second); err != nil || acquired {
		t.Fatalf("acquiring a held job lock: acquired=%v err=%v, want false/nil", acquired, err)
	}
	if got := idleSessionTimeoutOnPooledConn(t, single); got != "0" {
		t.Fatalf("idle_session_timeout of the connection returned after a failed acquire = %q, want \"0\"", got)
	}

	// (2) 取得して release した経路。release はこのコネクションをプールへ返さない。
	lock.release()
	released, acquired, err := acquireIngestJobLock(ctx, single, jobID, time.Second)
	if err != nil || !acquired {
		t.Fatalf("acquiring the job lock after release: acquired=%v err=%v", acquired, err)
	}
	released.release()
	if got := idleSessionTimeoutOnPooledConn(t, single); got != "0" {
		t.Fatalf("idle_session_timeout of the connection after release = %q, want \"0\"", got)
	}
}

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
// heartbeat が止まらないことを固定する。旧設計（rel_path の DB advisory lock）では
// 「heartbeat 停止 = markLost = 転送キャンセル」という終端判断だったので閾値で
// 止める理由があったが、新設計の heartbeat の唯一の仕事は job lock の lease を
// 更新することである（型の doc コメント参照）。一過性失敗で lease の更新を自分から
// 止めると、jobLockIdleSessionTimeout の経過後に Postgres がセッションを終了させて
// advisory lock が外れる（record_sweep が生存中の running 行を discard → 重複
// ジョブ投入 → temp replay を迂回した不要な再転送）。
func TestIngestJobLock_TransientHeartbeatFailuresNeverStop(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	l := newJobLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		return false, false, transientErr
	}

	for i := 1; i <= 50; i++ {
		if l.heartbeatTick() {
			t.Fatalf("heartbeatTick stopped after transient failure %d; transient failures must never stop the lease renewal", i)
		}
	}
}

// TestIngestJobLock_PermanentHeartbeatFailureStops は、コネクション切断
// （permanent）を検知したら即座に heartbeat を止めることを固定する。
func TestIngestJobLock_PermanentHeartbeatFailureStops(t *testing.T) {
	permanentErr := errors.New("simulated closed connection")
	l := newJobLock(nil, 1, "test")
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
	l := newJobLock(nil, 1, "test")
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
	l := newJobLock(nil, 1, "test")
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
