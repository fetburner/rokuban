// Package pgsource implements notifier's Postgres LISTEN source.
package pgsource

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

const notifyChannel = "rokuban"

// Source は Postgres の LISTEN/NOTIFY から notifier payload を読み取る。
type Source struct {
	pool *pgxpool.Pool
}

// New は Postgres の LISTEN/NOTIFY を使う Source を生成する。
func New(pool *pgxpool.Pool) *Source {
	return &Source{pool: pool}
}

// Listen は LISTEN を確立し、接続が切れるか ctx が終わるまで payload を out へ送る。
func (s *Source) Listen(ctx context.Context, ready func(), out chan<- string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("LISTEN %s: %w", notifyChannel, err)
	}
	slog.Info("event hub: listening", "channel", notifyChannel)
	ready()

	// 通知待ちは専用 goroutine に出す。終了時は cancel → goroutine の終了待ち →
	// conn.Release の順にし、goroutine が使用中の接続を pool に戻さない。
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()

	waitErr := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				waitErr <- err
				return
			}
			select {
			case out <- n.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case err := <-waitErr:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("waiting for notification: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
