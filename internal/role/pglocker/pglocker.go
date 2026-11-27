// Package pglocker は PostgreSQL の pg_try_advisory_lock で role.Locker を実装する。
// pgx の依存をここに閉じ込めるため、role 本体は pgx を import しない。
package pglocker

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/role"
)

// New は pgxpool からロック専用の接続を専有する role.Locker を返す。戻り値を role.Locker にして
// 具象型を隠すのは、呼び出し元（cmd/rokuban の方言の分岐）が方言ごとの型に依存しないようにするため。
func New(pool *pgxpool.Pool) role.Locker {
	return pgLocker{pool: pool}
}

type pgLocker struct{ pool *pgxpool.Pool }

func (l pgLocker) Checkout(ctx context.Context) (role.Session, error) {
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
