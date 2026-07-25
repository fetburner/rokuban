package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

// TestLockKey はテスト用データベースを排他するための advisory lock キー。
//
// テストはデータベースを共有し、各テストが MigrateReset で作り直す。go test ./... は
// パッケージを並行実行するため、マイグレーションを踏み合わないようこのキーで直列化する。
// db パッケージに置いているのは、testutil が db を参照する片方向の依存にするため。
// role パッケージのリーダー選出キー（fnv64a("rokuban:<role>")）とは衝突しない値を使う。
const TestLockKey int64 = -0x726f6b7562616e

// MigrateUp はアプリ (goose) → River の順にマイグレーションを適用する。
// River は独自のマイグレーション管理 (rivermigrate) を持つため goose とは別系統で実行する。
func MigrateUp(ctx context.Context, dbURL string) error {
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	}); err != nil {
		return err
	}
	return runRiverMigration(ctx, dbURL, rivermigrate.DirectionUp)
}

// MigrateDown はアプリ (goose) のマイグレーションを 1 ステップ戻す。
// River スキーマは rivermigrate が独立にバージョン管理しているため触らない。
func MigrateDown(ctx context.Context, dbURL string) error {
	return runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.Down(ctx)
		return err
	})
}

// MigrateReset はアプリ (goose) のマイグレーションをすべて巻き戻す。
func MigrateReset(ctx context.Context, dbURL string) error {
	return runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.DownTo(ctx, 0)
		return err
	})
}

func runGooseMigration(ctx context.Context, dbURL string, fn func(context.Context, *goose.Provider) error) error {
	subFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("getting migrations sub-FS: %w", err)
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("opening database for migration: %w", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, subFS)
	if err != nil {
		return fmt.Errorf("creating goose provider: %w", err)
	}

	if err := fn(ctx, provider); err != nil {
		return fmt.Errorf("running migration: %w", err)
	}

	return nil
}

func runRiverMigration(ctx context.Context, dbURL string, direction rivermigrate.Direction) error {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("opening pool for river migration: %w", err)
	}
	defer pool.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("creating river migrator: %w", err)
	}

	// River の down はデフォルト 1 ステップだけ戻す。全テーブルを削除するため -1 を指定。
	var opts *rivermigrate.MigrateOpts
	if direction == rivermigrate.DirectionDown {
		opts = &rivermigrate.MigrateOpts{TargetVersion: -1}
	}

	if _, err := migrator.Migrate(ctx, direction, opts); err != nil {
		return fmt.Errorf("running river migration: %w", err)
	}

	return nil
}
