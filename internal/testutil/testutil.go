package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
)

// DatabaseURL は ROKUBAN_TEST_DATABASE_URL を返す。未設定なら Skip する。
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

// invalidDBNameChar はデータベース識別子に使えない文字（英数と `_` 以外）にマッチする。
var invalidDBNameChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// postgresIdentifierMaxBytes は Postgres の識別子長の上限（バイト）。
const postgresIdentifierMaxBytes = 63

// packageTestDB は 1 プロセス（1 テストバイナリ = 1 パッケージ）につき 1 回だけ用意する
// テスト用データベースの状態を保持する。
var packageTestDB struct {
	once sync.Once
	url  string
	err  error
}

// SetupDB はテストパッケージ専用のデータベースに全テーブルを空の状態で用意し、
// 接続プールを返す。テスト終了時にプールを Close する。
//
// テスト用データベースはパッケージごとに分ける。go test ./... はパッケージを並行実行
// するため、全パッケージで 1 つの DB を共有すると advisory lock による直列化が必要になり、
// 待ち時間が lock_timeout を超えて flaky になっていた。ROKUBAN_TEST_DATABASE_URL から
// テストバイナリ名で導出した専用データベースを使うことで、待ち合わせなしで並行実行できる。
//
// データベースの作成とマイグレーションはプロセスごとに 1 回だけ行い（sync.Once）、
// 各 SetupDB 呼び出しでは全テーブルを TRUNCATE するだけにして高速化する。
func SetupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	base := DatabaseURL(t)

	packageTestDB.once.Do(func() {
		packageTestDB.url, packageTestDB.err = ensurePackageTestDatabase(ctx, base)
	})
	if packageTestDB.err != nil {
		t.Fatalf("preparing package test database: %v", packageTestDB.err)
	}

	if err := truncateAllTables(ctx, packageTestDB.url); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}

	pool, err := pgxpool.New(ctx, packageTestDB.url)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// packageTestDatabaseName は base のデータベース名とテストバイナリ名から、
// パッケージ固有のデータベース名を決定的に導出する。英数と `_` 以外の文字は `_` に
// 置換し、Postgres の識別子長 63 バイト制限に収まるよう必要なら切り詰める。
func packageTestDatabaseName(base *url.URL) string {
	binary := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	name := strings.TrimPrefix(base.Path, "/") + "_" + binary
	name = invalidDBNameChar.ReplaceAllString(name, "_")
	if len(name) > postgresIdentifierMaxBytes {
		name = name[:postgresIdentifierMaxBytes]
	}
	return name
}

// ensurePackageTestDatabase は base の DB へ接続してパッケージ専用データベースを
// DROP → CREATE し、マイグレーションを適用したうえで、そのデータベースを指す URL を返す。
func ensurePackageTestDatabase(ctx context.Context, base string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing base database url: %w", err)
	}

	// データベース名はプレースホルダで渡せないため、正規化済みの名前だけを使い
	// pgx.Identifier で引用してから埋め込む。
	name := packageTestDatabaseName(baseURL)
	quoted := pgx.Identifier{name}.Sanitize()

	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return "", fmt.Errorf("connecting to base database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
		return "", fmt.Errorf("dropping package test database: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return "", fmt.Errorf("creating package test database: %w", err)
	}

	dbURL := *baseURL
	dbURL.Path = "/" + name
	dbURLStr := dbURL.String()

	if err := db.MigrateUp(ctx, dbURLStr); err != nil {
		return "", fmt.Errorf("migrating package test database: %w", err)
	}
	return dbURLStr, nil
}

// truncateAllTables は public スキーマのユーザーテーブル（goose の管理テーブル
// goose_db_version を除く）をすべて TRUNCATE し、シーケンスも 1 から振り直す。
// River のテーブル（river_job など）もマイグレーションで作られる通常のテーブルなので
// 対象に含まれる。
func truncateAllTables(ctx context.Context, dbURL string) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connecting to package test database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
	`)
	if err != nil {
		return fmt.Errorf("listing tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning table name: %w", err)
		}
		tables = append(tables, pgx.Identifier{name}.Sanitize())
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating tables: %w", err)
	}
	if len(tables) == 0 {
		return nil
	}

	if _, err := conn.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		return fmt.Errorf("truncating tables: %w", err)
	}
	return nil
}
