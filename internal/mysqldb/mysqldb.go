// Package mysqldb は、MySQL の接続と migration を管理する。
// PostgreSQL の internal/db と同じ層に置き、方言固有の設定と初期スキーマを store から分ける。
package mysqldb

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"
)

const (
	connectionCollation = "utf8mb4_0900_as_cs"
	connectionIsolation = "READ-COMMITTED"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// DSN は接続先（host:port）・認証・DB 名から OpenPool に渡す DSN を組む。
// 値のエスケープは go-sql-driver に任せ、呼び出し側は引用符を付けない。
func DSN(addr, user, password, database string) string {
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.User = user
	cfg.Passwd = password
	cfg.DBName = database
	return cfg.FormatDSN()
}

// OpenPool は MySQL の接続プールを開き、設定を検査する。DSN の DB 名、接続先、
// 認証情報は保ち、接続ごとの設定は store 契約の値へ置き換える。
// TestOpenPoolEnforcesConnectionSettings が接続後の値を検査する。
func OpenPool(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing MySQL DSN: %w", err)
	}
	if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return nil, fmt.Errorf("setting MySQL connection charset: %w", err)
	}
	cfg.Loc = time.UTC
	cfg.ParseTime = true
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	// go-sql-driver v1.10.1 の照合順序表に `utf8mb4_0900_as_cs` がなく、mysql.Charset で指定すると接続が unknown collation で失敗する。
	// 公開されたシステム変数用 DSN パラメーターで設定し、TestOpenPoolEnforcesConnectionSettings が接続値を検査する。
	cfg.Params["collation_connection"] = "'" + connectionCollation + "'"
	cfg.Params["transaction_isolation"] = "'" + connectionIsolation + "'"
	cfg.Params["sql_mode"] = "'TRADITIONAL'"
	cfg.Params["time_zone"] = "'+00:00'"

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("opening MySQL pool: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to MySQL: %w", err)
	}
	if err := CheckConnectionSettings(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("validating MySQL connection settings: %w", err)
	}
	return db, nil
}

// CheckConnectionSettings は接続プールから取得した接続の設定を検査する。
// OpenPool の起動時検査にも使い、TestCheckConnectionSettingsRejectsEachMismatch が
// 不一致を検出する経路を検査する。
func CheckConnectionSettings(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("checking out MySQL connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var isolation, sqlMode, collation, timeZone string
	if err := conn.QueryRowContext(ctx, `
		SELECT @@transaction_isolation, @@sql_mode, @@collation_connection, @@time_zone
	`).Scan(&isolation, &sqlMode, &collation, &timeZone); err != nil {
		return fmt.Errorf("reading MySQL connection settings: %w", err)
	}
	if isolation != connectionIsolation {
		return fmt.Errorf("MySQL transaction_isolation %q does not match required setting", isolation)
	}
	if !hasTraditionalSQLMode(sqlMode) {
		return fmt.Errorf("MySQL sql_mode does not satisfy TRADITIONAL mode requirements")
	}
	if collation != connectionCollation {
		return fmt.Errorf("MySQL collation_connection %q does not match required setting", collation)
	}
	if timeZone != "+00:00" {
		return fmt.Errorf("MySQL time_zone %q does not match required setting", timeZone)
	}
	return nil
}

// hasTraditionalSQLMode は TRADITIONAL とそれが含意するモードを確認する。
// 表示されるモード全体の一致は求めず、TestOpenPoolEnforcesConnectionSettings が実際の接続値を検査する。
func hasTraditionalSQLMode(raw string) bool {
	modes := make(map[string]struct{})
	for _, mode := range strings.Split(raw, ",") {
		modes[mode] = struct{}{}
	}
	for _, required := range []string{
		"TRADITIONAL",
		"STRICT_TRANS_TABLES",
		"STRICT_ALL_TABLES",
		"NO_ZERO_IN_DATE",
		"NO_ZERO_DATE",
		"ERROR_FOR_DIVISION_BY_ZERO",
		"NO_ENGINE_SUBSTITUTION",
	} {
		if _, ok := modes[required]; !ok {
			return false
		}
	}
	return true
}

// CheckMigrationsCurrent は DB の適用済み最大バージョンが、埋め込みの最新と一致しなければエラーを返す。
// PostgreSQL の db.CheckMigrationsCurrent と同じ判定（goose の GetVersions）である。
func CheckMigrationsCurrent(ctx context.Context, db *sql.DB) error {
	subFS, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("getting MySQL migrations sub-FS: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, subFS)
	if err != nil {
		return fmt.Errorf("creating MySQL goose provider: %w", err)
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("reading migration versions: %w", err)
	}
	if current != target {
		return fmt.Errorf("database schema is at migration %d, but this binary embeds migration %d; run migrate up first", current, target)
	}
	return nil
}

// MigrateUp はこのパッケージに埋め込んだ MySQL 用 goose migration を適用する。
// MySQL のスキーマは PostgreSQL から複製せず、独自の初期 migration から始める。
func MigrateUp(ctx context.Context, db *sql.DB) error {
	subFS, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("getting MySQL migrations sub-FS: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, subFS)
	if err != nil {
		return fmt.Errorf("creating MySQL goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("applying MySQL migrations: %w", err)
	}
	return nil
}

// MigrateDown は MySQL 用 goose migration を 1 ステップ戻す。PostgreSQL の db.MigrateDown と同じ操作である。
func MigrateDown(ctx context.Context, db *sql.DB) error {
	subFS, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("getting MySQL migrations sub-FS: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, subFS)
	if err != nil {
		return fmt.Errorf("creating MySQL goose provider: %w", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		return fmt.Errorf("rolling back MySQL migration: %w", err)
	}
	return nil
}

// MigrateDownTo は埋め込みの migration を版 version まで戻す。後続の migration が増えても、
// 検査したい版を確実に戻すための操作である。PostgreSQL の db.MigrateDownTo に当たる。
func MigrateDownTo(ctx context.Context, db *sql.DB, version int64) error {
	subFS, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("getting MySQL migrations sub-FS: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, subFS)
	if err != nil {
		return fmt.Errorf("creating MySQL goose provider: %w", err)
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		return fmt.Errorf("rolling back MySQL migrations to %d: %w", version, err)
	}
	return nil
}

// IsUniqueViolation は err が MySQL の一意キー違反かを返す。
// TestRunTxMapsUniqueViolationToConflict が store.ErrConflict への変換を検査する。
func IsUniqueViolation(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
