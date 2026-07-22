package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
)

// NewPool は接続プールを作成し、Ping で疎通確認する。
// 接続失敗を起動時に即検出する (EPGStation#628 の教訓: エラーを握り潰さない)。
func NewPool(ctx context.Context, cfg config.DBConfig) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	return pool, nil
}
