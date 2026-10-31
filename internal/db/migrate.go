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

// MigrateUp はアプリの goose マイグレーションを適用し、全 migration の適用後に
// DB の文字分類を検査する。新規 DB で migrate が最初に触る経路なので、
// 誤ったロケールのままアプリを起動させない。
func MigrateUp(ctx context.Context, dbURL string) error {
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	}); err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connecting to validate database locale after migration: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := checkLocaleSupport(ctx, conn); err != nil {
		return fmt.Errorf("validating database after migration: %w", err)
	}
	return nil
}

// CheckMigrationsCurrent は DB の適用済み最大バージョンが、このバイナリに埋め込まれた
// 最新の migration と一致しなければエラーを返す。エラーには両方のバージョンを書く。
//
// 判定は goose の公開 API `Provider.GetVersions`（DB の最大バージョンと埋め込みの最新）による。
// DB が一度もマイグレーションされていないと、この呼び出しは `goose_db_version` を作る
// （`migrate up` が作るのと同じテーブル）。アプリの表は触らない。
func CheckMigrationsCurrent(ctx context.Context, dbURL string) error {
	var current, target int64
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		var err error
		current, target, err = p.GetVersions(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("reading migration versions: %w", err)
	}
	if current != target {
		return fmt.Errorf("database schema is at migration %d, but this binary embeds migration %d; run migrate up first", current, target)
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
