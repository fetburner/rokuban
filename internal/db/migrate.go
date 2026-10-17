package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// MigrateUp はアプリの goose マイグレーションを適用し、pg_trgm の日本語類似度を検査する。
// 新規 DB では migrate が最初に触る経路なので、worker の起動より前にロケール不適合を検出する。
func MigrateUp(ctx context.Context, dbURL string) error {
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	}); err != nil {
		return err
	}

	// migrate は新しいデータベースへ最初に接続する経路なので、拡張を作った直後に
	// 実際の pg_trgm の挙動を検査し、誤ったロケールのままアプリを起動させない。
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connecting to validate database locale after migration: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := checkPgTrgmJapaneseSupport(ctx, conn); err != nil {
		return fmt.Errorf("validating database locale after migration: %w", err)
	}
	return nil
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
