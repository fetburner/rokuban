package worker

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ingestJobLockKeyPrefix と encodeJobLockKeyPrefix は、それぞれのジョブの
	// プロセス生存確認用 advisory lock の名前空間。ジョブ ID ごとにキーを分け、
	// 他のロールの advisory lock と衝突しないようにする。
	ingestJobLockKeyPrefix = "rokuban:ingest:job:"
	encodeJobLockKeyPrefix = "rokuban:encode:job:"

	// defaultJobLockTimeout は lock 用コネクションの取得と
	// pg_try_advisory_lock の両方に与える既定の上限。
	defaultJobLockTimeout = 10 * time.Second

	// jobLockHeartbeatInterval は、長時間のジョブ中も lock 用セッションを idle に
	// しないための疎通間隔。これは zombie job を止めるためではなく、回収側が
	// 生存中のジョブをプロセス死と誤判定しないための keepalive である。
	jobLockHeartbeatInterval = time.Second

	// jobLockHeartbeatTimeout は heartbeat のクエリ 1 回あたりの応答待ち上限。
	jobLockHeartbeatTimeout = 2 * time.Second
)

const jobLockHeldQuery = `
SELECT EXISTS (
    SELECT 1
    FROM pg_locks
    WHERE locktype = 'advisory'
      AND pid = pg_backend_pid()
      AND classid = (($1::bigint >> 32) & 4294967295)::oid
      AND objid = ($1::bigint & 4294967295)::oid
      AND objsubid = 1
      AND granted
)`

// advisoryLockKey は名前空間と値から pg_try_advisory_lock(bigint) 用のキーを作る。
// Postgres の hashtext() は安定性を保証された API ではないため、Go 側の FNV-1a
// を使う。衝突しても不要な再試行になるだけで、DB の一意制約による採用結果は
// 変わらない。
func advisoryLockKey(prefix, value string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix + value))
	return int64(h.Sum64())
}

func jobLockKey(prefix string, jobID int64) int64 {
	return advisoryLockKey(prefix, strconv.FormatInt(jobID, 10))
}

func ingestJobLockKey(jobID int64) int64 {
	return jobLockKey(ingestJobLockKeyPrefix, jobID)
}

func encodeJobLockKey(jobID int64) int64 {
	return jobLockKey(encodeJobLockKeyPrefix, jobID)
}

// jobLock はジョブの process-death 検出用セッション lock と、そのセッションを
// idle 切断から守る keepalive を所有する。
//
// lock の利用目的（転送先や出力の排他など）は各ワーカー側で定義する。heartbeat
// が lock 喪失を検知しても、長時間処理の context をキャンセルする責務は持たない。
type jobLock struct {
	conn  *pgxpool.Conn
	key   int64
	label string

	stopHeartbeat chan struct{}
	heartbeatDone chan struct{}
	releaseOnce   sync.Once

	// checkHeldFunc は heartbeat の一過性/恒久エラーを注入するテストフック。
	// nil の場合は実 DB を照会する。
	checkHeldFunc func() (held, permanent bool, err error)
}

func newJobLock(conn *pgxpool.Conn, key int64, label string) *jobLock {
	return &jobLock{conn: conn, key: key, label: label}
}

// ingestJobLock は既存の ingest テストと呼び出し側の名前を保つ互換 alias。
type ingestJobLock = jobLock

func newIngestJobLock(conn *pgxpool.Conn, key int64, label string) *jobLock {
	return newJobLock(conn, key, label)
}

func (l *jobLock) startHeartbeat() {
	if l.stopHeartbeat != nil {
		return
	}
	l.stopHeartbeat = make(chan struct{})
	l.heartbeatDone = make(chan struct{})
	go l.heartbeatLoop()
}

func (l *jobLock) heartbeatLoop() {
	defer close(l.heartbeatDone)

	ticker := time.NewTicker(jobLockHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopHeartbeat:
			return
		case <-ticker.C:
			if l.heartbeatTick() {
				return
			}
		}
	}
}

// heartbeatTick は heartbeat 1 回分の判定を行う。true を返した場合は、以後の
// heartbeat を止める。ただし長時間処理や recovery の transaction をキャンセルする
// 責務は持たない。
//
// このループの唯一の仕事は job lock 用セッションを idle 切断から守る keepalive
// である（型の doc コメント、docs/recording/ingest.md 参照）。一過性の DB
// エラーではループを止めない --- 止めると keepalive が失われてセッションが idle
// のまま放置され、pgbouncer 等の server_idle_timeout で切断されて advisory lock
// が解放される。生存中のジョブをプロセス死と誤認すると、回収側が古い running 行を
// discard して代替ジョブを投入し、処理を二重実行することになる。止めるのは
// permanent（コネクション切断）と !held（lock 喪失の確定）のときだけ。
func (l *jobLock) heartbeatTick() bool {
	check := l.checkHeldFunc
	if check == nil {
		check = l.checkHeldAndClassify
	}

	held, permanent, err := check()
	if err != nil {
		if permanent {
			slog.Warn("job advisory lock heartbeat connection lost", "job", l.label, "err", err)
			return true
		}
		slog.Warn("job advisory lock heartbeat check failed transiently; continuing", "job", l.label, "err", err)
		return false
	}

	if !held {
		slog.Warn("job advisory lock was lost; continuing with worker-specific protocol", "job", l.label)
		return true
	}
	return false
}

func (l *jobLock) checkHeldAndClassify() (held, permanent bool, err error) {
	held, err = l.checkHeld()
	if err == nil {
		return held, false, nil
	}
	return false, l.isPermanentCheckHeldError(), err
}

func (l *jobLock) isPermanentCheckHeldError() bool {
	return l.conn == nil || l.conn.Conn().IsClosed()
}

func (l *jobLock) checkHeld() (bool, error) {
	if l.conn == nil {
		return false, fmt.Errorf("job advisory lock has no connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobLockHeartbeatTimeout)
	defer cancel()
	var held bool
	if err := l.conn.QueryRow(ctx, jobLockHeldQuery, l.key).Scan(&held); err != nil {
		return false, err
	}
	return held, nil
}

// stopHeartbeatLoop は recovery のように lock 用セッション自身で短い DB transaction
// を実行する呼び出し側が、同じ pgx connection への並行利用を避けるために使う。
func (l *jobLock) stopHeartbeatLoop() {
	if l.stopHeartbeat == nil {
		return
	}
	close(l.stopHeartbeat)
	<-l.heartbeatDone
	l.stopHeartbeat = nil
	l.heartbeatDone = nil
}

func (l *jobLock) release() {
	l.releaseOnce.Do(func() {
		l.stopHeartbeatLoop()
		if l.conn == nil {
			return
		}

		unlockCtx, cancel := context.WithTimeout(context.Background(), defaultJobLockTimeout)
		defer cancel()
		var stillHeld bool
		if err := l.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&stillHeld); err != nil {
			slog.Warn("failed to release job advisory lock", "job", l.label, "err", err)
		} else if !stillHeld {
			// pgxpool の Release がセッション状態を暗黙にリセットすると仮定せず、
			// 明示 unlock の結果を確認する。
			slog.Warn("job advisory lock was already lost before release", "job", l.label)
		}
		l.conn.Release()
	})
}

// acquireJobLock は Work の開始時にジョブ ID のセッションレベル advisory lock を
// 取得し、取得したセッションを keepalive 付きで返す。回収側も同じ prefix のキーを
// pg_try_advisory_lock で試し、取得できた場合に限って元プロセスが死んでセッション
// が切れたと確定する。
func acquireJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration, prefix, label string) (*jobLock, bool, error) {
	if timeout <= 0 {
		timeout = defaultJobLockTimeout
	}

	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := pool.Acquire(acquireCtx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring connection for %s advisory lock: %w", label, err)
	}

	key := jobLockKey(prefix, jobID)
	var acquired bool
	if err := conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("trying %s advisory lock: %w", label, err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	lock := newJobLock(conn, key, label)
	lock.startHeartbeat()
	return lock, true, nil
}

// acquireIngestJobLock は ingest 用の job-id advisory lock を取得する。
func acquireIngestJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*jobLock, bool, error) {
	return acquireJobLock(ctx, pool, jobID, timeout, ingestJobLockKeyPrefix, fmt.Sprintf("ingest job %d", jobID))
}

// acquireEncodeJobLock は encode 用の job-id advisory lock を取得する。
func acquireEncodeJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*jobLock, bool, error) {
	return acquireJobLock(ctx, pool, jobID, timeout, encodeJobLockKeyPrefix, fmt.Sprintf("encode job %d", jobID))
}
