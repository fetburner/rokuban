package role

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func lockKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("rokuban:" + name))
	return int64(h.Sum64())
}

// TryAcquire は pg_try_advisory_lock でシングルトンロールのリーダー選出を行う。
// advisory lock はセッションレベルなので、Acquire したコネクションを保持し続ける限り
// ロックが維持される。プロセス死亡時はコネクション切断で自動解放される。
func TryAcquire(ctx context.Context, pool *pgxpool.Pool, role string) (acquired bool, release func(), err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("acquiring connection for leader lock: %w", err)
	}

	key := lockKey(role)
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return false, nil, fmt.Errorf("trying advisory lock for %s: %w", role, err)
	}

	if !acquired {
		conn.Release()
		slog.Info("role already held by another process", "role", role)
		return false, nil, nil
	}

	slog.Info("acquired leader lock", "role", role)
	release = func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
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

// RunSingleton はシングルトンロールの監督ループを実行する。
// pg_try_advisory_lock を定期試行し、取得したら roleFunc を child context で起動する。
// リーダー中はロック専用コネクションに定期 heartbeat を送り、失敗したらロールを停止して
// 取得ループに戻る。ctx がキャンセルされるまでブロックする。
func RunSingleton(ctx context.Context, pool *pgxpool.Pool, role string, roleFunc func(ctx context.Context) error, cfg *SingletonConfig) error {
	if cfg == nil {
		d := defaultSingletonConfig()
		cfg = &d
	}
	key := lockKey(role)

	for {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to acquire connection for leader lock", "role", role, "err", err)
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		var acquired bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
			conn.Release()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to try advisory lock", "role", role, "err", err)
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		if !acquired {
			conn.Release()
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		slog.Info("acquired leader lock", "role", role)

		runErr := runWithHeartbeat(ctx, conn, role, cfg.HeartbeatInterval, roleFunc)

		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
		slog.Info("released leader lock", "role", role)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if runErr != nil {
			slog.Warn("role exited with error, will retry", "role", role, "err", runErr)
		}
	}
}

func runWithHeartbeat(ctx context.Context, conn *pgxpool.Conn, role string, heartbeatInterval time.Duration, roleFunc func(ctx context.Context) error) error {
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
			var n int
			err := conn.QueryRow(hbCtx, "SELECT 1").Scan(&n)
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
