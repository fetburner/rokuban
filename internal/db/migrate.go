package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

// MigrateUp はアプリ (goose) → River の順にマイグレーションを適用する。
// River は独自のマイグレーション管理 (rivermigrate) を持つため goose とは別系統で実行する。
func MigrateUp(ctx context.Context, dbURL string) error {
	if err := runGooseMigration(dbURL, func(db *sql.DB) error {
		return goose.Up(db, "migrations")
	}); err != nil {
		return err
	}
	return runRiverMigration(ctx, dbURL, rivermigrate.DirectionUp)
}

// MigrateDown はアプリ (goose) のマイグレーションを 1 ステップ戻す。
// River スキーマは rivermigrate が独立にバージョン管理しているため触らない。
func MigrateDown(ctx context.Context, dbURL string) error {
	return runGooseMigration(dbURL, func(db *sql.DB) error {
		return goose.Down(db, "migrations")
	})
}

func runGooseMigration(dbURL string, fn func(*sql.DB) error) error {
	goose.SetBaseFS(migrations)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("setting dialect: %w", err)
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("opening database for migration: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := fn(db); err != nil {
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
