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

// ingestJobLockKeyPrefix は ingest ジョブのプロセス生存確認用 advisory lock の
// 名前空間。ジョブ ID ごとにキーを分け、他のロールの advisory lock と衝突しない
// ようにする。Work の開始から終了まで同じ PostgreSQL セッションで保持する。
const ingestJobLockKeyPrefix = "rokuban:ingest:job:"

// defaultIngestJobLockTimeout は lock 用コネクションの取得と
// pg_try_advisory_lock の両方に与える既定の上限。
const defaultIngestJobLockTimeout = 10 * time.Second

// ingestJobLockHeartbeatInterval は、長時間の ingest 中も job lock 用セッションを
// idle にしないための疎通間隔。これは zombie ingest を止めるためではなく、
// record_sweep が生存中のジョブをプロセス死と誤判定しないための keepalive である。
const ingestJobLockHeartbeatInterval = time.Second

// ingestJobLockHeartbeatTimeout は heartbeat のクエリ 1 回あたりの応答待ち上限。
const ingestJobLockHeartbeatTimeout = 2 * time.Second

// ingestJobLockMaxTransientFailures は一過性の heartbeat 失敗を連続して許す回数。
// DB の短いレイテンシで転送を不必要に再試行させないため、接続断や lock 喪失と
// 別に扱う。
const ingestJobLockMaxTransientFailures = 3

const ingestJobLockHeldQuery = `
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

func ingestJobLockKey(jobID int64) int64 {
	return advisoryLockKey(ingestJobLockKeyPrefix, strconv.FormatInt(jobID, 10))
}

// ingestJobLock は ingest ジョブの process-death 検出用セッション lock と、その
// セッションを idle 切断から守る keepalive を所有する。
//
// これは転送先の排他ではない。ingest のバイトは canonical path と同じディレクトリ
// の試行固有一時ファイルへ書かれ、DB の一意 INSERT が採用を決める。そのため
// heartbeat が lock 喪失を検知しても転送 context はキャンセルしない。古い実行が
// 一時ファイルへ書き続けても canonical file を壊せず、DB commit が競合を決着する。
type ingestJobLock struct {
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

func newIngestJobLock(conn *pgxpool.Conn, key int64, label string) *ingestJobLock {
	return &ingestJobLock{conn: conn, key: key, label: label}
}

func (l *ingestJobLock) startHeartbeat() {
	if l.stopHeartbeat != nil {
		return
	}
	l.stopHeartbeat = make(chan struct{})
	l.heartbeatDone = make(chan struct{})
	go l.heartbeatLoop()
}

func (l *ingestJobLock) heartbeatLoop() {
	defer close(l.heartbeatDone)

	ticker := time.NewTicker(ingestJobLockHeartbeatInterval)
	defer ticker.Stop()

	var consecutiveFailures int
	for {
		select {
		case <-l.stopHeartbeat:
			return
		case <-ticker.C:
			if l.heartbeatTick(&consecutiveFailures) {
				return
			}
		}
	}
}

// heartbeatTick は heartbeat 1 回分の判定を行う。true を返した場合は、以後の
// heartbeat を止める。ただし ingest の転送や commit をキャンセルする責務は持たない。
func (l *ingestJobLock) heartbeatTick(consecutiveFailures *int) bool {
	check := l.checkHeldFunc
	if check == nil {
		check = l.checkHeldAndClassify
	}

	held, permanent, err := check()
	if err != nil {
		if permanent {
			slog.Warn("ingest: job advisory lock heartbeat connection lost", "job", l.label, "err", err)
			return true
		}
		*consecutiveFailures++
		if *consecutiveFailures >= ingestJobLockMaxTransientFailures {
			slog.Warn("ingest: job advisory lock heartbeat stopped after transient failures", "job", l.label, "err", err, "consecutive_failures", *consecutiveFailures)
			return true
		}
		slog.Warn("ingest: job advisory lock heartbeat check failed transiently, retrying", "job", l.label, "err", err, "consecutive_failures", *consecutiveFailures)
		return false
	}

	*consecutiveFailures = 0
	if !held {
		slog.Warn("ingest: job advisory lock was lost; continuing with temporary-file protocol", "job", l.label)
		return true
	}
	return false
}

func (l *ingestJobLock) checkHeldAndClassify() (held, permanent bool, err error) {
	held, err = l.checkHeld()
	if err == nil {
		return held, false, nil
	}
	return false, l.isPermanentCheckHeldError(), err
}

func (l *ingestJobLock) isPermanentCheckHeldError() bool {
	return l.conn == nil || l.conn.Conn().IsClosed()
}

func (l *ingestJobLock) checkHeld() (bool, error) {
	if l.conn == nil {
		return false, fmt.Errorf("job advisory lock has no connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ingestJobLockHeartbeatTimeout)
	defer cancel()
	var held bool
	if err := l.conn.QueryRow(ctx, ingestJobLockHeldQuery, l.key).Scan(&held); err != nil {
		return false, err
	}
	return held, nil
}

// stopHeartbeatLoop は recovery のように lock 用セッション自身で短い DB transaction
// を実行する呼び出し側が、同じ pgx connection への並行利用を避けるために使う。
func (l *ingestJobLock) stopHeartbeatLoop() {
	if l.stopHeartbeat == nil {
		return
	}
	close(l.stopHeartbeat)
	<-l.heartbeatDone
	l.stopHeartbeat = nil
	l.heartbeatDone = nil
}

func (l *ingestJobLock) release() {
	l.releaseOnce.Do(func() {
		l.stopHeartbeatLoop()
		if l.conn == nil {
			return
		}

		unlockCtx, cancel := context.WithTimeout(context.Background(), defaultIngestJobLockTimeout)
		defer cancel()
		var stillHeld bool
		if err := l.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&stillHeld); err != nil {
			slog.Warn("ingest: failed to release job advisory lock", "job", l.label, "err", err)
		} else if !stillHeld {
			// pgxpool の Release がセッション状態を暗黙にリセットすると仮定せず、
			// 明示 unlock の結果を確認する。
			slog.Warn("ingest: job advisory lock was already lost before release", "job", l.label)
		}
		l.conn.Release()
	})
}

// acquireIngestJobLock は Work の開始時にジョブ ID のセッションレベル advisory lock
// を取得し、取得したセッションを keepalive 付きで返す。record_sweep の回収側も同じ
// キーを pg_try_advisory_lock で試し、取得できた場合に限って元プロセスが死んで
// セッションが切れたと確定する。
func acquireIngestJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*ingestJobLock, bool, error) {
	if timeout <= 0 {
		timeout = defaultIngestJobLockTimeout
	}

	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := pool.Acquire(acquireCtx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring connection for ingest job advisory lock: %w", err)
	}

	key := ingestJobLockKey(jobID)
	var acquired bool
	if err := conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("trying ingest job advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	lock := newIngestJobLock(conn, key, fmt.Sprintf("ingest job %d", jobID))
	lock.startHeartbeat()
	return lock, true, nil
}
