package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	// ハードコードしていたときは、ローカルでロール名が違うと必ず落ち、
	// パッケージ専用 DB を使うようになった後はマイグレーションしていない別 DB を
	// 指してしまって CI でも落ちた。
	cfg := dbConfigFromURL(t, dbURL)

	pool, err := NewPool(ctx, cfg, nil, 0)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var latestVersion int64
	err = pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&latestVersion)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if latestVersion <= 0 {
		t.Errorf("latest goose migration version = %d, want a positive version", latestVersion)
	}

	var schemaInfoExists bool
	err = pool.QueryRow(ctx, "SELECT to_regclass('public.schema_info') IS NOT NULL").Scan(&schemaInfoExists)
	if err != nil {
		t.Fatalf("checking schema_info: %v", err)
	}
	if schemaInfoExists {
		t.Error("schema_info still exists")
	}

	var constraintDefinition string
	err = pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conname = 'recording_encode_policy_recording_id_fkey'
	`).Scan(&constraintDefinition)
	if err != nil {
		t.Fatalf("checking recording_encode_policy FK: %v", err)
	}
	if !strings.Contains(constraintDefinition, "ON DELETE CASCADE") {
		t.Errorf("recording_encode_policy FK = %q, want ON DELETE CASCADE", constraintDefinition)
	}

	var ruleIndexExists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = 'recordings_rule_id_idx'
		)
	`).Scan(&ruleIndexExists)
	if err != nil {
		t.Fatalf("checking recordings rule_id index: %v", err)
	}
	if !ruleIndexExists {
		t.Error("recordings_rule_id_idx does not exist")
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
	_, err := NewPool(ctx, cfg, nil, 0)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

// TestNewPool_PoolerCompatFailFast_DoesNotDial は pooler_compat と worker ロールの
// 組み合わせが、実際に DB へ接続を試みる前に（=ホストが到達不能でも即座に）
// エラーになることを確認する。TryAcquire 相当のチェックが NewPool の先頭で
// 行われている（buildPoolConfig で pgxpool.NewWithConfig より前に検査する）ことの
// 回帰テスト。チェックを NewWithConfig の後段に動かすと、到達不能ホストへの接続
// タイムアウト（数十秒）が発生してこのテストがタイムアウトで落ちる。
func TestNewPool_PoolerCompatFailFast_DoesNotDial(t *testing.T) {
	cfg := config.DBConfig{
		Host:         "10.255.255.1", // ルーティングされない予約アドレス（到達不能を意図）
		Port:         5432,
		User:         "u",
		Password:     "p",
		Database:     "d",
		SSLMode:      "disable",
		PoolerCompat: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := NewPool(ctx, cfg, []string{"worker"}, 0)
	if err == nil {
		t.Fatal("expected fail-fast error for pooler_compat + worker, got nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NewPool tried to dial the unreachable host instead of failing fast: %v", err)
	}
}

// TestNewPool_APIStatementTimeout_Enforced は db.api_statement_timeout が実際に
// クエリを打ち切ることを、pg_sleep で意図的に超過させて確認する
// （CLAUDE.md テスト規律: 設定値を読んだだけのテストにしない）。
func TestNewPool_APIStatementTimeout_Enforced(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		_ = MigrateDown(ctx, dbURL)
	})

	cfg := dbConfigFromURL(t, dbURL)
	cfg.APIStatementTimeout = 200 * time.Millisecond

	t.Run("api role: statement_timeout aborts a slow query", func(t *testing.T) {
		pool, err := NewPool(ctx, cfg, []string{"api"}, 0)
		if err != nil {
			t.Fatalf("NewPool: %v", err)
		}
		defer pool.Close()

		_, err = pool.Exec(ctx, "SELECT pg_sleep(2)")
		if err == nil {
			t.Fatal("expected pg_sleep(2) to be aborted by statement_timeout, got nil error")
		}
		if !strings.Contains(err.Error(), "statement timeout") {
			t.Errorf("error = %v, want it to mention statement timeout", err)
		}
	})

	t.Run("no api role: the same slow query is not aborted", func(t *testing.T) {
		pool, err := NewPool(ctx, cfg, []string{"worker"}, 0)
		if err != nil {
			t.Fatalf("NewPool: %v", err)
		}
		defer pool.Close()

		// api_statement_timeout（200ms）が worker ロールには効かないことを、
		// それを大きく超える 1 秒の pg_sleep が成功することで確認する。
		if _, err := pool.Exec(ctx, "SELECT pg_sleep(1)"); err != nil {
			t.Errorf("pg_sleep(1) should succeed without the api role, got: %v", err)
		}
	})
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
