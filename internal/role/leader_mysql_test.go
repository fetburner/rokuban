package role

import (
	"context"
	"database/sql"
	"fmt"
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

// startMySQLElector は pool を持つ elector を 1 つ起動する。roleFunc は、リーダーになったら
// active を立て、ctx が切れるまで待つ。
func startMySQLElector(ctx context.Context, name string, pool *sql.DB, role string, cfg *SingletonConfig) *electorState {
	e := &electorState{name: name, done: make(chan error, 1)}
	go func() {
		e.done <- RunSingletonWith(ctx, NewMySQLLocker(pool), role, func(ctx context.Context) error {
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
	a := startMySQLElector(ctx, "a", poolA, role, cfg)
	b := startMySQLElector(ctx, "b", poolB, role, cfg)
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

// TestMySQLLeaderTakeoverAfterConnectionKill は、リーダーのロック接続を切ると、リーダーが
// リーダーシップを手放し、別の接続がロックを引き継ぐことを確かめる。リーダーは専有したロック接続で
// heartbeat するので、接続を切られた時点で退かなければならない。
//
// 引き継ぐのは、必ずしももう一方の elector とは限らない。切られたほうも次の poll でロックを取り直せる。
// 検査するのは「元の接続が手放され、ロックが別の接続に移ったこと」と「同時に 2 つのリーダーが無いこと」である。
func TestMySQLLeaderTakeoverAfterConnectionKill(t *testing.T) {
	db := mysqltest.SetupDB(t)
	poolA := mysqltest.OpenSiblingPool(t, db)
	poolB := mysqltest.OpenSiblingPool(t, db)
	role := t.Name()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := &SingletonConfig{PollInterval: 50 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond}
	a := startMySQLElector(ctx, "a", poolA, role, cfg)
	b := startMySQLElector(ctx, "b", poolB, role, cfg)
	t.Cleanup(func() {
		cancel()
		<-a.done
		<-b.done
	})

	if !waitUntil(func() bool { return a.leader.Load() != b.leader.Load() }) {
		t.Fatal("no elector became leader within the timeout")
	}
	leader := a
	if b.leader.Load() {
		leader = b
	}
	ownerID, err := leaderLockOwnerID(ctx, db, role)
	if err != nil {
		t.Fatal(err)
	}
	endedBefore := leader.ended.Load()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", ownerID)); err != nil {
		t.Fatalf("killing the leader's lock connection: %v", err)
	}

	// 接続を切られたリーダーは、リーダーシップを手放さなければならない。
	if !waitUntil(func() bool { return leader.ended.Load() > endedBefore }) {
		t.Fatalf("leader %s kept leadership after its lock connection was killed", leader.name)
	}
	// ロックは切れた接続から離れ、別の接続が取る。
	if !waitUntil(func() bool {
		id, err := leaderLockOwnerID(ctx, db, role)
		return err == nil && id != ownerID
	}) {
		t.Fatal("no other connection took the lock after the leader's lock connection was killed")
	}
	// 引き継いだ後も、リーダーは 1 つだけ。ロックを取ってからリーダーの状態を立てるまでの僅かな間があるので待つ。
	if !waitUntil(func() bool { return countLeaders(a, b) == 1 }) {
		t.Fatalf("after takeover, leaders = %d, want exactly 1", countLeaders(a, b))
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
