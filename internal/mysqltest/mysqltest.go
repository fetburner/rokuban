// Package mysqltest は、MySQL store のテスト用 DB を作成する。
// PostgreSQL の internal/testutil と対になる置き場にし、MySQL の DB 作成と破棄を集約する。
package mysqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/fetburner/rokuban/internal/mysqldb"
)

var databaseSequence atomic.Uint64

// SetupDB はテスト専用 DB を作り、MySQL 用の初期 migration を適用する。
// テスト終了時に DB を削除する。DSN はサーバーを指し、テスト自身は変更しない。
// テストごとに DB を分けるので、呼び出し元の t.Parallel と両立する。
func SetupDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := TestDSN(t)
	db, name, err := newDatabase(context.Background(), dsn)
	if err != nil {
		t.Fatalf("creating migrated MySQL test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing MySQL test pool: %v", err)
		}
		dropDatabase(t, dsn, name)
	})
	return db
}

// TestDSN は MySQL の統合テスト用 DSN を返す。
// ROKUBAN_REQUIRE_MYSQL が有効なら DSN 未設定を失敗にし、それ以外は skip する。
func TestDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("ROKUBAN_TEST_MYSQL_DSN")
	if dsn != "" {
		return dsn
	}
	if os.Getenv("ROKUBAN_REQUIRE_MYSQL") == "1" {
		t.Fatal("ROKUBAN_TEST_MYSQL_DSN is required when ROKUBAN_REQUIRE_MYSQL=1")
	}
	t.Skip("ROKUBAN_TEST_MYSQL_DSN not set")
	return ""
}

func newDatabase(ctx context.Context, dsn string) (*sql.DB, string, error) {
	name := nextDatabaseName()
	admin, err := mysqldb.OpenPool(ctx, dsn)
	if err != nil {
		return nil, "", err
	}
	_, createErr := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs")
	closeErr := admin.Close()
	if createErr != nil {
		return nil, "", errors.Join(fmt.Errorf("creating MySQL test database: %w", createErr), closeErr)
	}
	if closeErr != nil {
		return nil, "", joinCleanupError(ctx, dsn, name, fmt.Errorf("closing MySQL admin pool: %w", closeErr))
	}
	databaseDSN, err := dsnForDatabase(dsn, name)
	if err != nil {
		return nil, "", joinCleanupError(ctx, dsn, name, fmt.Errorf("building test database DSN: %w", err))
	}
	db, err := mysqldb.OpenPool(ctx, databaseDSN)
	if err != nil {
		return nil, "", joinCleanupError(ctx, dsn, name, fmt.Errorf("opening MySQL test database: %w", err))
	}
	if err := mysqldb.MigrateUp(ctx, db); err != nil {
		closeErr := db.Close()
		migrationErr := fmt.Errorf("migrating MySQL test database: %w", err)
		if closeErr != nil {
			migrationErr = errors.Join(migrationErr, fmt.Errorf("closing MySQL test pool: %w", closeErr))
		}
		return nil, "", joinCleanupError(ctx, dsn, name, migrationErr)
	}
	return db, name, nil
}

func joinCleanupError(ctx context.Context, dsn, name string, err error) error {
	cleanupErr := dropDatabaseContext(ctx, dsn, name)
	if cleanupErr == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("cleaning up MySQL test database: %w", cleanupErr))
}

func dsnForDatabase(dsn, name string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing MySQL DSN: %w", err)
	}
	cfg.DBName = name
	return cfg.FormatDSN(), nil
}

// OpenSiblingPool は db と同じテスト DB に、別のプールを開く。2 つのプールは別プロセスの
// 接続に相当する（リーダー選出のテストが、プロセスごとのプールを模すために使う）。
// プールはテスト終了時に閉じる。
func OpenSiblingPool(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatalf("reading test database name: %v", err)
	}
	dsn, err := dsnForDatabase(TestDSN(t), name)
	if err != nil {
		t.Fatalf("building sibling DSN: %v", err)
	}
	pool, err := mysqldb.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("opening sibling MySQL pool: %v", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing sibling MySQL pool: %v", err)
		}
	})
	return pool
}

func nextDatabaseName() string {
	return "rokuban_test_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatUint(databaseSequence.Add(1), 10)
}

func dropDatabase(t *testing.T, dsn, name string) {
	t.Helper()
	if err := dropDatabaseContext(context.Background(), dsn, name); err != nil {
		t.Errorf("dropping MySQL test database %q: %v", name, err)
	}
}

func dropDatabaseContext(ctx context.Context, dsn, name string) error {
	admin, err := mysqldb.OpenPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("opening MySQL admin pool to drop database: %w", err)
	}
	if !strings.HasPrefix(name, "rokuban_test_") {
		return errors.Join(fmt.Errorf("refusing unexpected MySQL test database name %q", name), admin.Close())
	}
	_, dropErr := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`")
	closeErr := admin.Close()
	if dropErr != nil {
		dropErr = fmt.Errorf("dropping database: %w", dropErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("closing MySQL admin pool: %w", closeErr)
	}
	return errors.Join(dropErr, closeErr)
}
