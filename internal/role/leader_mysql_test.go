package role

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
)

// electorState は 1 つのプロセスに相当する elector の、リーダー状態を表す。
type electorState struct {
	name   string
	leader atomic.Bool
	// ended は、リーダーシップを手放した回数。
	ended atomic.Int32
	done  chan error
}

// startElector は locker を持つ elector を 1 つ起動する。roleFunc は、リーダーになったら
// leader を立て、ctx が切れるまで待つ。
func startElector(ctx context.Context, name string, locker Locker, role string, cfg *SingletonConfig) *electorState {
	e := &electorState{name: name, done: make(chan error, 1)}
	go func() {
		e.done <- RunSingletonWith(ctx, locker, role, func(ctx context.Context) error {
			e.leader.Store(true)
			defer func() {
				e.leader.Store(false)
				e.ended.Add(1)
			}()
			<-ctx.Done()
			return nil
		}, cfg)
	}()
	return e
}

// gatedLocker は Checkout を止められる Locker。リーダーのプロセスが死んだ、または
// ネットワークから切れた状態を、テストの中で再現する（死んだプロセスは再取得しない）。
type gatedLocker struct {
	inner Locker
	down  atomic.Bool
}

func (g *gatedLocker) Checkout(ctx context.Context) (Session, error) {
	if g.down.Load() {
		return nil, errors.New("process is down (test gate)")
	}
	return g.inner.Checkout(ctx)
}

// mysqlWaitTimeout は、リーダーの交代や退出を待つ上限。heartbeat と poll の間隔（50ms）に比べて十分長い。
const mysqlWaitTimeout = 5 * time.Second

// waitUntil は cond が真になるまで mysqlWaitTimeout の間待つ。真にならなければ false を返す。
func waitUntil(cond func() bool) bool {
	deadline := time.Now().Add(mysqlWaitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// leaderLockOwnerID は GET_LOCK を保持しているサーバー側のセッション（接続 ID）を返す。
// performance_schema の metadata_locks が、ユーザー定義ロックの保持スレッドを示す。
func leaderLockOwnerID(ctx context.Context, admin *sql.DB, role string) (int64, error) {
	var ids []int64
	rows, err := admin.QueryContext(ctx, `
		SELECT t.PROCESSLIST_ID
		FROM performance_schema.metadata_locks m
		JOIN performance_schema.threads t ON t.THREAD_ID = m.OWNER_THREAD_ID
		WHERE m.OBJECT_TYPE = 'USER LEVEL LOCK' AND m.OBJECT_NAME = ? AND m.LOCK_STATUS = 'GRANTED'`,
		mysqlLockName(role))
	if err != nil {
		return 0, fmt.Errorf("querying lock owner: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scanning lock owner: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating lock owners: %w", err)
	}
	if len(ids) != 1 {
		return 0, fmt.Errorf("expected exactly one owner of the election lock, got %d", len(ids))
	}
	return ids[0], nil
}

// TestMySQLLeaderElectionIsExclusive は、2 つの elector のうち必ず 1 つだけがリーダーになることを確かめる。
func TestMySQLLeaderElectionIsExclusive(t *testing.T) {
	db := mysqltest.SetupDB(t)
	poolA := mysqltest.OpenSiblingPool(t, db)
	poolB := mysqltest.OpenSiblingPool(t, db)
	role := t.Name()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := &SingletonConfig{PollInterval: 50 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond}
	a := startElector(ctx, "a", NewMySQLLocker(poolA), role, cfg)
	b := startElector(ctx, "b", NewMySQLLocker(poolB), role, cfg)
	// 終了を待ってからプールを閉じる。GET_LOCK の名前はサーバー全体で共有されるので、
	// 前のテストのセッションが残ったまま次のテストが同じ名前を取らないようにする。
	t.Cleanup(func() {
		cancel()
		<-a.done
		<-b.done
	})

	if !waitUntil(func() bool { return countLeaders(a, b) == 1 }) {
		t.Fatalf("no elector became leader within the timeout (leaders=%d)", countLeaders(a, b))
	}
	// 保持の間、リーダーが 2 つになる瞬間が無いことを確かめる。
	for range 40 {
		if n := countLeaders(a, b); n != 1 {
			t.Fatalf("leaders = %d while the lock should be held by exactly one elector", n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestMySQLLeaderTakeoverAfterConnectionKill は、リーダーのロック接続を切ると、リーダーが退き、
// もう一方の elector が引き継ぐことを確かめる。リーダーのプロセスが死んだ状態を再現するため、
// リーダー側の Locker を切る（再取得させない）。ロックは切れた接続から離れ、もう一方だけが取れる。
//
// 検査は次のとおり。元のリーダーの roleFunc が終わること、もう一方がリーダーになること、
// 2 つのリーダーが同時に立つ時間が heartbeat の間隔（テストでは 50ms）の数倍を超えないこと。
// 最後の点は厳密な 0 ではない。ロックは接続が切れた時点でサーバーが解放するが、古いリーダーが
// 退いたと気付くのは次の heartbeat だからである。
func TestMySQLLeaderTakeoverAfterConnectionKill(t *testing.T) {
	db := mysqltest.SetupDB(t)
	poolA := mysqltest.OpenSiblingPool(t, db)
	poolB := mysqltest.OpenSiblingPool(t, db)
	role := t.Name()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := &SingletonConfig{PollInterval: 50 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond}
	gateA := &gatedLocker{inner: NewMySQLLocker(poolA)}
	gateB := &gatedLocker{inner: NewMySQLLocker(poolB)}
	a := startElector(ctx, "a", gateA, role, cfg)
	b := startElector(ctx, "b", gateB, role, cfg)
	t.Cleanup(func() {
		cancel()
		<-a.done
		<-b.done
	})

	if !waitUntil(func() bool { return countLeaders(a, b) == 1 }) {
		t.Fatal("no elector became leader within the timeout")
	}
	leader, other, gate := a, b, gateA
	if b.leader.Load() {
		leader, other, gate = b, a, gateB
	}

	// 同時に 2 つのリーダーが立っていた最長の連続時間を、標本化で測る。
	var overlapMax, overlapStart atomic.Int64
	stopSampling := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-stopSampling:
				return
			default:
			}
			now := time.Now().UnixNano()
			if countLeaders(a, b) == 2 {
				if overlapStart.Load() == 0 {
					overlapStart.Store(now)
				}
				if d := now - overlapStart.Load(); d > overlapMax.Load() {
					overlapMax.Store(d)
				}
			} else {
				overlapStart.Store(0)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ownerID, err := leaderLockOwnerID(ctx, db, role)
	if err != nil {
		t.Fatal(err)
	}
	endedBefore := leader.ended.Load()
	gate.down.Store(true)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", ownerID)); err != nil {
		t.Fatalf("killing the leader's lock connection: %v", err)
	}

	if !waitUntil(func() bool { return leader.ended.Load() > endedBefore }) {
		t.Fatalf("leader %s's roleFunc did not end after its lock connection was killed", leader.name)
	}
	if !waitUntil(func() bool { return other.leader.Load() }) {
		t.Fatalf("%s did not take over after the leader's lock connection was killed", other.name)
	}
	close(stopSampling)
	<-samplerDone
	if d := time.Duration(overlapMax.Load()); d > 500*time.Millisecond {
		t.Fatalf("two leaders were active for %v; the old leader should stop within a heartbeat", d)
	}
}

// countLeaders は、リーダーの状態が立っている elector の数を返す。
func countLeaders(electors ...*electorState) int {
	n := 0
	for _, e := range electors {
		if e.leader.Load() {
			n++
		}
	}
	return n
}

// TestMySQLLockNameAcceptsLongRole は、サイト名が最長（53 文字）の watcher のロール名でも
// GET_LOCK が名前を拒まず、ロックを取れることを確かめる。GET_LOCK の名前は 64 文字以内である。
func TestMySQLLockNameAcceptsLongRole(t *testing.T) {
	db := mysqltest.SetupDB(t)
	role := "watcher:" + strings.Repeat("s", 53)

	sess, err := NewMySQLLocker(db).Checkout(context.Background())
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	defer sess.Release()

	acquired, err := sess.TryLock(context.Background(), role)
	if err != nil {
		t.Fatalf("TryLock(%d-char role): %v", len(role), err)
	}
	if !acquired {
		t.Fatalf("TryLock(%d-char role) = false, want the lock to be free", len(role))
	}
}
