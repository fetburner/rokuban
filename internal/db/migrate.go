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

// CheckMediaCopyMigrationSafety は、物理コピー表を導入済みの DB が旧 writer と
// 互換性のない migration を通るとき、停止の明示確認を要求する。
func CheckMediaCopyMigrationSafety(ctx context.Context, dbURL string, mediaCopyWritersStopped bool) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connecting to inspect media copy migration state: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var versionTableExists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.goose_db_version') IS NOT NULL`).Scan(&versionTableExists); err != nil {
		return fmt.Errorf("checking migration version table: %w", err)
	}
	if !versionTableExists {
		return nil
	}

	var currentVersion int64
	if err := conn.QueryRow(ctx, `
SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0)
FROM public.goose_db_version`).Scan(&currentVersion); err != nil {
		return fmt.Errorf("reading current migration version: %w", err)
	}
	if mediaCopyMigrationRequiresStoppedWriters(currentVersion) && !mediaCopyWritersStopped {
		return fmt.Errorf("database version %d requires stopping all old media copy writers before migration; stop the old workers, then rerun migrate up with --media-copy-writers-stopped", currentVersion)
	}
	return nil
}

func mediaCopyMigrationRequiresStoppedWriters(version int64) bool {
	return version >= 37 && version < 40
}

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
