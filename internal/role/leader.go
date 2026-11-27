// Package role はシングルトンロールのリーダー選出を提供する。
//
// 「ソケットを connect し続ける」形のロール、すなわち watcher（mirakc の SSE を購読し続ける）
// のためにある。listen する側のロール（api / notifier / streamer）は水平にスケールするので
// 選出は不要で、ソケットを持たない仕事（ruler / reconciler / record_sweep / ingest / epg_sync）は
// ジョブとして走る。共通契約（jobq）は排他を保証せず、現行の重複抑止はジョブエンジン adapter の
// UniqueOpts による。ruler の並走による上書きは未解決（docs/data.md §2）。
//
// したがって RunSingleton の呼び出し元は cmd/rokuban/server.go の 1 箇所（watcher）だけだが、
// これは削除の根拠ではない（issue #24 M2-20、docs/operations.md §5）。呼び出し元が 1 つに
// 減ったのは機構が 1 つに減ったという成果であり、リーダー選出の失敗モード（heartbeat 喪失 /
// split-brain / フェイルオーバー遅延）を単独でテストできる価値は残る。
//
// なお watcher の singleton 性は「正しさ」の要件ではない。processRecord は record_sync の
// (site, record_id) 行ロックで冪等なので、複数の watcher が同じ record を処理しても recordings は
// 重複しない（docs/recording.md §3.3）。ここで選出しているのは「mirakc に N 本の SSE を張らない」
// という接続数の配慮であって、破れても壊れはしない。
package role

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Locker は方言ごとの排他ロックの実装を表す。Checkout が返す Session は、リーダーである間
// ずっと専有する。ロックは接続（セッション）に結びつき、接続が切れると解放される。
// PostgreSQL は pg_try_advisory_lock（NewPgLocker）、MySQL は GET_LOCK（NewMySQLLocker）。
type Locker interface {
	// Checkout は、ロックに使う接続を 1 本専有して返す。
	Checkout(ctx context.Context) (Session, error)
}

// Session は専有した 1 本の接続上のロック操作。Release は、ロックを解放した後に 1 度だけ呼ぶ。
type Session interface {
	// TryLock は role のロックを待たずに取り、取れたかを返す。
	TryLock(ctx context.Context, role string) (bool, error)
	// Unlock は role のロックを解放する。失敗したら接続を捨て、セッションごとロックを手放す。
	Unlock(ctx context.Context, role string) error
	// Ping は、ロックを保持している接続そのものに疎通確認を送る（heartbeat）。
	Ping(ctx context.Context) error
	// Release は専有した接続を返す。ロックを保持したまま返してはならない。
	Release()
}

// NewPgLocker は pgxpool からロック専用の接続を専有する Locker を返す。
func NewPgLocker(pool *pgxpool.Pool) Locker {
	return pgLocker{pool: pool}
}

type pgLocker struct{ pool *pgxpool.Pool }

func (l pgLocker) Checkout(ctx context.Context) (Session, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring connection for leader lock: %w", err)
	}
	return pgSession{conn: conn}, nil
}

type pgSession struct{ conn *pgxpool.Conn }

func pgLockKey(role string) int64 {
	h := fnv.New64a()
	h.Write([]byte("rokuban:" + role))
	return int64(h.Sum64())
}

func (s pgSession) TryLock(ctx context.Context, role string) (bool, error) {
	var acquired bool
	if err := s.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", pgLockKey(role)).Scan(&acquired); err != nil {
		return false, fmt.Errorf("trying advisory lock for %s: %w", role, err)
	}
	return acquired, nil
}

func (s pgSession) Unlock(ctx context.Context, role string) error {
	if _, err := s.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", pgLockKey(role)); err != nil {
		return fmt.Errorf("releasing advisory lock for %s: %w", role, err)
	}
	return nil
}

func (s pgSession) Ping(ctx context.Context) error {
	var n int
	return s.conn.QueryRow(ctx, "SELECT 1").Scan(&n)
}

func (s pgSession) Release() { s.conn.Release() }

// NewMySQLLocker は MySQL の接続プールからロック専用の接続を専有する Locker を返す。
// GET_LOCK はセッション単位で、同じ接続で 2 回取ると解放も 2 回要る。接続をプールに戻すと、
// 他の処理がその接続で同じ名前のロックを取り、ロックを保持したまま成功しうる。そのため
// リーダーである間は接続を返さない。
func NewMySQLLocker(db *sql.DB) Locker {
	return mysqlLocker{db: db}
}

type mysqlLocker struct{ db *sql.DB }

func (l mysqlLocker) Checkout(ctx context.Context) (Session, error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring connection for leader lock: %w", err)
	}
	return mysqlSession{conn: conn}, nil
}

type mysqlSession struct{ conn *sql.Conn }

// mysqlLockName は GET_LOCK の名前を返す。名前は 64 文字以内でなければ拒まれ、watcher のロール名は
// サイト名（最長 53 文字）を含むので、ロール名をそのまま使うと長いサイト名で失敗する。そこで PostgreSQL の
// 鍵と同じ 64 ビットのハッシュを固定長で使う（長さは 8 + 16 文字で、入力に依らない）。
func mysqlLockName(role string) string {
	h := fnv.New64a()
	h.Write([]byte("rokuban:" + role))
	return fmt.Sprintf("rokuban:%016x", h.Sum64())
}

func (s mysqlSession) TryLock(ctx context.Context, role string) (bool, error) {
	var got sql.NullInt64
	if err := s.conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", mysqlLockName(role)).Scan(&got); err != nil {
		return false, fmt.Errorf("trying GET_LOCK for %s: %w", role, err)
	}
	// 1 は取得、0 は他者が保持、NULL はエラー。
	return got.Valid && got.Int64 == 1, nil
}

func (s mysqlSession) Unlock(ctx context.Context, role string) error {
	var got sql.NullInt64
	err := s.conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", mysqlLockName(role)).Scan(&got)
	if err == nil && got.Valid && got.Int64 == 1 {
		return nil
	}
	// 解放できなかったら、ロックを持つセッションを残さない。(*sql.Conn).Raw の doc comment が定める
	// とおり、f が driver.ErrBadConn を返すと、それ以降 Conn は使えず Close で接続を捨てる。
	// サーバー側のセッションが終わって GET_LOCK が解放される（自分で RELEASE_LOCK を重ねない）。
	_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil {
		return fmt.Errorf("releasing GET_LOCK for %s: %w", role, err)
	}
	return fmt.Errorf("releasing GET_LOCK for %s: lock was not held by this session", role)
}

func (s mysqlSession) Ping(ctx context.Context) error { return s.conn.PingContext(ctx) }

func (s mysqlSession) Release() { _ = s.conn.Close() }

// TryAcquire は pg_try_advisory_lock でシングルトンロールのリーダー選出を行う。
// advisory lock はセッションレベルなので、Acquire したコネクションを保持し続ける限り
// ロックが維持される。プロセス死亡時はコネクション切断で自動解放される。
func TryAcquire(ctx context.Context, pool *pgxpool.Pool, role string) (acquired bool, release func(), err error) {
	sess, err := NewPgLocker(pool).Checkout(ctx)
	if err != nil {
		return false, nil, err
	}

	acquired, err = sess.TryLock(ctx, role)
	if err != nil {
		sess.Release()
		return false, nil, err
	}

	if !acquired {
		sess.Release()
		slog.Info("role already held by another process", "role", role)
		return false, nil, nil
	}

	slog.Info("acquired leader lock", "role", role)
	release = func() {
		_ = sess.Unlock(context.Background(), role)
		sess.Release()
	}
	return true, release, nil
}

// SingletonConfig は RunSingleton の設定。
type SingletonConfig struct {
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
}

func defaultSingletonConfig() SingletonConfig {
	return SingletonConfig{
		PollInterval:      15 * time.Second,
		HeartbeatInterval: 10 * time.Second,
	}
}

const heartbeatTimeout = 5 * time.Second

// RunSingleton は PostgreSQL のロックで RunSingletonWith を呼ぶ。
func RunSingleton(ctx context.Context, pool *pgxpool.Pool, role string, roleFunc func(ctx context.Context) error, cfg *SingletonConfig) error {
	return RunSingletonWith(ctx, NewPgLocker(pool), role, roleFunc, cfg)
}

// RunSingletonWith はシングルトンロールの監督ループを実行する。
// locker のロックを定期試行し、取得したら roleFunc を child context で起動する。
// リーダー中はロックを保持する接続に定期 heartbeat を送り、失敗したらロールを停止して
// 取得ループに戻る。ctx がキャンセルされるまでブロックする。
func RunSingletonWith(ctx context.Context, locker Locker, role string, roleFunc func(ctx context.Context) error, cfg *SingletonConfig) error {
	if cfg == nil {
		d := defaultSingletonConfig()
		cfg = &d
	}

	for {
		sess, err := locker.Checkout(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to acquire connection for leader lock", "role", role, "err", err)
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		acquired, err := sess.TryLock(ctx, role)
		if err != nil {
			sess.Release()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to try advisory lock", "role", role, "err", err)
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		if !acquired {
			sess.Release()
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		slog.Info("acquired leader lock", "role", role)

		runErr := runWithHeartbeat(ctx, sess, role, cfg.HeartbeatInterval, roleFunc)

		_ = sess.Unlock(context.Background(), role)
		sess.Release()
		slog.Info("released leader lock", "role", role)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if runErr != nil {
			slog.Warn("role exited with error, will retry", "role", role, "err", runErr)
		}
	}
}

func runWithHeartbeat(ctx context.Context, sess Session, role string, heartbeatInterval time.Duration, roleFunc func(ctx context.Context) error) error {
	roleCtx, roleCancel := context.WithCancel(ctx)
	defer roleCancel()

	roleDone := make(chan error, 1)
	go func() {
		roleDone <- roleFunc(roleCtx)
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case err := <-roleDone:
			return err

		case <-ticker.C:
			hbCtx, hbCancel := context.WithTimeout(context.Background(), heartbeatTimeout)
			err := sess.Ping(hbCtx)
			hbCancel()
			if err != nil {
				if ctx.Err() != nil {
					roleCancel()
					return <-roleDone
				}
				slog.Error("heartbeat failed, releasing leadership", "role", role, "err", err)
				roleCancel()
				return <-roleDone
			}

		case <-ctx.Done():
			roleCancel()
			return <-roleDone
		}
	}
}

func sleepWithJitter(ctx context.Context, base time.Duration) {
	jitter := time.Duration(rand.Int64N(int64(base/2))) - base/4
	select {
	case <-time.After(base + jitter):
	case <-ctx.Done():
	}
}
