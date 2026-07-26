package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/config"
)

// invalidTestDBNameChar はデータベース識別子に使えない文字（英数と `_` 以外）にマッチする。
var invalidTestDBNameChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// postgresIdentifierMaxBytes は Postgres の識別子長の上限（バイト）。
const postgresIdentifierMaxBytes = 63

// dbPkgTestDB は db パッケージのテスト専用データベースの状態を保持する。
// プロセス（db.test バイナリ）につき 1 回だけ用意する。
var dbPkgTestDB struct {
	once sync.Once
	url  string
	err  error
}

// testDatabaseURL は db パッケージのマイグレーションテスト専用データベースの URL を返す。
// ROKUBAN_TEST_DATABASE_URL が未設定なら Skip する。
//
// internal/testutil はマイグレーション適用済みのデータベースを前提に db パッケージへ
// 依存しており、db パッケージのテストから testutil を使うと循環インポートになる。
// そのため testutil.SetupDB と同じ考え方（テストバイナリ名から導出した専用データベースを
// 用意する）をここで独自に実装する。ここで作るデータベースは他パッケージのテスト DB とは
// バイナリ名（db.test）で名前が分かれるため衝突しない。マイグレーション自体を検証する
// テストなので、testutil のように TRUNCATE では済ませず、各テストが直接
// MigrateUp / MigrateDown / MigrateReset を呼ぶ。
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}

	dbPkgTestDB.once.Do(func() {
		dbPkgTestDB.url, dbPkgTestDB.err = createTestDatabase(context.Background(), base)
	})
	if dbPkgTestDB.err != nil {
		t.Fatalf("preparing test database: %v", dbPkgTestDB.err)
	}
	return dbPkgTestDB.url
}

// createTestDatabase は base の DB へ接続し、db パッケージのテスト専用データベースを
// DROP → CREATE してその URL を返す。データベース名はプレースホルダで渡せないため、
// 正規化済みの名前だけを使い pgx.Identifier で引用してから埋め込む。
func createTestDatabase(ctx context.Context, base string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing base database url: %w", err)
	}

	binary := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	name := strings.TrimPrefix(baseURL.Path, "/") + "_" + binary
	name = invalidTestDBNameChar.ReplaceAllString(name, "_")
	if len(name) > postgresIdentifierMaxBytes {
		name = name[:postgresIdentifierMaxBytes]
	}
	quoted := pgx.Identifier{name}.Sanitize()

	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return "", fmt.Errorf("connecting to base database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
		return "", fmt.Errorf("dropping test database: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return "", fmt.Errorf("creating test database: %w", err)
	}

	dbURL := *baseURL
	dbURL.Path = "/" + name
	return dbURL.String(), nil
}

func TestMigrateUpDown(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
}

func TestNewPool(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		_ = MigrateDown(ctx, dbURL)
	})

	// 接続先はハードコードせず、実際に migrate した DB の URL から組む。
	// ハードコードしていたときは (a) ローカルでロール名が違うと必ず落ち、
	// (b) パッケージ専用 DB を使うようになった後は schema_info が無い別 DB を
	// 指してしまって CI でも落ちた。
	cfg := dbConfigFromURL(t, dbURL)

	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var value string
	err = pool.QueryRow(ctx, "SELECT value FROM schema_info WHERE key = 'version'").Scan(&value)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if value != "1" {
		t.Errorf("schema_info version = %q, want %q", value, "1")
	}
}

func TestNewPool_ConnectionFailure(t *testing.T) {
	cfg := config.DBConfig{
		Host:     "localhost",
		Port:     59999,
		User:     "nonexistent",
		Password: "nonexistent",
		Database: "nonexistent",
		SSLMode:  "disable",
	}

	ctx := context.Background()
	_, err := NewPool(ctx, cfg)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

// dbConfigFromURL は接続 URL を config.DBConfig に変換する。
// DBConfig → DSN → プールという経路そのものを検証したいので、URL を直接
// pgxpool に渡すのではなく DBConfig を経由させる。
func dbConfigFromURL(t *testing.T, raw string) config.DBConfig {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing database url: %v", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			t.Fatalf("parsing port %q: %v", p, err)
		}
	}
	password, _ := u.User.Password()
	sslMode := u.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "disable"
	}
	return config.DBConfig{
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		Password: password,
		Database: strings.TrimPrefix(u.Path, "/"),
		SSLMode:  sslMode,
	}
}
