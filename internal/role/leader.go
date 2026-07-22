package role

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"

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
		// pool への Release() だけではセッションが維持されロックが残るため、明示的に unlock する
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
	}
	return true, release, nil
}
