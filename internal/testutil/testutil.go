package testutil

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
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	rivermigrate "github.com/fetburner/rokuban/internal/jobq/riveradapter/migrate"
)

// DatabaseURL は、このテストのデータベースの接続 URL を返す。未設定なら Skip する。
//
// 返す DB は SetupDB と同じ、このテスト専用のデータベースである。
func DatabaseURL(t *testing.T) string {
	t.Helper()
	return testDBOf(t).url
}

// invalidDBNameChar はデータベース識別子に使えない文字（英数と `_` 以外）にマッチする。
var invalidDBNameChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// postgresIdentifierMaxBytes は Postgres の識別子長の上限（バイト）。
const postgresIdentifierMaxBytes = 63

// templateStrategy は CREATE DATABASE ... TEMPLATE の STRATEGY。
// 手元と CI の測定で決めた値を入れる（PR 本文の実測を参照）。
const templateStrategy = "FILE_COPY"

// templateDB は 1 プロセス（1 テストバイナリ = 1 パッケージ）につき 1 回だけ用意する
// テンプレートデータベースの状態を保持する。テンプレートはマイグレーション済みで、
// マイグレーションが入れた行も TRUNCATE で消した空の状態。以後は接続しない。
var templateDB struct {
	once sync.Once
	name string
	err  error
}

// testDBSeq は、同一プロセス内でテスト用データベースの名前を一意にする連番。
var testDBSeq atomic.Uint64

// testDB は 1 テストが使うデータベースの状態。1 テストに 1 つ作り、そのテストの
// SetupDB / DatabaseURL / DatabaseConfig はすべて同じ DB を指す。
type testDB struct {
	name string
	url  string
	// handed は SetupDB が一度でもこの DB のプールを返したことを表す。
	// 2 回目以降の SetupDB は DB を空にしてから返す（呼ぶたびに空の DB、という契約を保つ）。
	handed bool
}

// testDBsByTest は *testing.T ごとに作った DB を保持する。テストの終了時（t.Cleanup）に
// DB を DROP してエントリも消す。
var testDBsByTest = struct {
	sync.Mutex
	m map[testing.TB]*testDB
}{m: map[testing.TB]*testDB{}}

// SetupDB は、このテスト専用の空のデータベースに接続するプールを返す。
// テスト終了時にプールを閉じ、データベースを DROP する。
//
// データベースはテンプレート（マイグレーション済み・空）から CREATE DATABASE ... TEMPLATE
// で複製する。テストごとに別の DB を使うので、テストどうしは行を共有せず、
// t.Parallel() で並列に走らせられる。テンプレートの作成はプロセスごとに 1 回だけ行う（sync.Once）。
//
// 同じテストで 2 回呼ぶと、2 回目は DB を空にしてから返す。1 回目のプールも同じ DB を見る。
// 1 回目のプールから見れば、2 回目の呼び出しで行が消えたように見える。
func SetupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	tdb := testDBOf(t)

	testDBsByTest.Lock()
	reuse := tdb.handed
	tdb.handed = true
	testDBsByTest.Unlock()

	if reuse {
		if err := truncateAllTables(context.Background(), tdb.url); err != nil {
			t.Fatalf("truncating tables: %v", err)
		}
	}
	pool, err := pgxpool.New(context.Background(), tdb.url)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// DatabaseConfig はこのテストのデータベースの接続設定を返す（SetupDB と同じ DB）。
//
// プールではなく config.DBConfig が要るのは、**config ファイルを書いて
// cmd/rokuban の RunE を実際に走らせるテスト**のため（起動時の配線は
// resolve*/validate* を直接呼ぶテストでは検証できない）。SetupDB より前に呼んでも
// 後に呼んでもよい。どちらも同じテストの DB を指す。
func DatabaseConfig(t *testing.T) config.DBConfig {
	t.Helper()
	cfg, err := dbConfigFromURL(testDBOf(t).url)
	if err != nil {
		t.Fatalf("parsing test database url: %v", err)
	}
	return cfg
}

// testDBOf は t の DB を返す。まだ無ければ作り、t の Cleanup で DROP を登録する。
func testDBOf(t *testing.T) *testDB {
	t.Helper()
	base := envDatabaseURL(t)

	testDBsByTest.Lock()
	cur := testDBsByTest.m[t]
	testDBsByTest.Unlock()
	if cur != nil {
		return cur
	}

	tdb, err := createTestDB(context.Background(), base)
	if err != nil {
		t.Fatalf("creating test database: %v", err)
	}

	testDBsByTest.Lock()
	testDBsByTest.m[t] = tdb
	testDBsByTest.Unlock()

	t.Cleanup(func() {
		testDBsByTest.Lock()
		delete(testDBsByTest.m, t)
		testDBsByTest.Unlock()
		if err := dropTestDB(context.Background(), base, tdb.name); err != nil {
			t.Errorf("dropping test database: %v", err)
		}
	})
	return tdb
}

// envDatabaseURL は ROKUBAN_TEST_DATABASE_URL を返す。未設定なら Skip する。
// この URL が指すデータベースは直接使わない。テストごとの DB はここから作る。
func envDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

// dbConfigFromURL は接続 URL を config.DBConfig に変換する。DatabaseConfig が
// config ファイル用に config.DBConfig を組むために使う。
func dbConfigFromURL(raw string) (config.DBConfig, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return config.DBConfig{}, fmt.Errorf("parsing database url: %w", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return config.DBConfig{}, fmt.Errorf("parsing port %q: %w", p, err)
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
	}, nil
}

// packagePrefix は base のデータベース名とテストバイナリ名から、パッケージ固有の接頭辞を
// 決定的に導出する。英数と `_` 以外の文字は `_` に置換する。長さは呼び出し側で切る。
func packagePrefix(base *url.URL) string {
	binary := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	name := strings.TrimPrefix(base.Path, "/") + "_" + binary
	return invalidDBNameChar.ReplaceAllString(name, "_")
}

// truncateBytes は s を max バイト以内に切る。Postgres の識別子は ASCII 前提でよい
// （packagePrefix の結果は英数と `_` だけ）。
func truncateBytes(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// ensureTemplate は base の DB へ接続し、テンプレート DB を作り直してマイグレーションを
// 適用し、マイグレーションが入れた行を TRUNCATE で消す。完了後は ALLOW_CONNECTIONS を
// false にして、以後の接続を Postgres 側で拒否させる。テンプレートの名前を返す。
//
// マイグレーションの各関数は自分の接続を閉じて戻る（db.MigrateUp は sql.DB と pgx.Connect、
// rivermigrate.Up は pgxpool.Pool を defer で閉じる）。CREATE DATABASE ... TEMPLATE は
// テンプレートに他の接続が無いことを要求するため、これを前提に順序を組んでいる。
func ensureTemplate(ctx context.Context, base string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing base database url: %w", err)
	}
	name := truncateBytes(packagePrefix(baseURL)+"_tmpl", postgresIdentifierMaxBytes)
	quoted := pgx.Identifier{name}.Sanitize()

	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return "", fmt.Errorf("connecting to base database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
		return "", fmt.Errorf("dropping template database: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return "", fmt.Errorf("creating template database: %w", err)
	}

	tmplURL := *baseURL
	tmplURL.Path = "/" + name
	tmplURLStr := tmplURL.String()

	if err := db.MigrateUp(ctx, tmplURLStr); err != nil {
		return "", fmt.Errorf("migrating template database: %w", err)
	}
	if err := rivermigrate.Up(ctx, tmplURLStr); err != nil {
		return "", fmt.Errorf("migrating River schema: %w", err)
	}
	if err := truncateAllTables(ctx, tmplURLStr); err != nil {
		return "", fmt.Errorf("clearing template database: %w", err)
	}

	if _, err := conn.Exec(ctx, "ALTER DATABASE "+quoted+" WITH ALLOW_CONNECTIONS false"); err != nil {
		return "", fmt.Errorf("disallowing connections to template database: %w", err)
	}
	return name, nil
}

// createTestDB は base の DB へ接続し、テンプレートから新しい空の DB を複製する。
// 名前は接頭辞 + pid + 連番で、接頭辞は識別子長 63 バイトに収まるよう切る。
// 切るのは接頭辞だけなので、pid と連番で一意性は保たれる。
func createTestDB(ctx context.Context, base string) (*testDB, error) {
	tmpl, err := templateDBName(ctx, base)
	if err != nil {
		return nil, err
	}

	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parsing base database url: %w", err)
	}
	suffix := fmt.Sprintf("_%d_%d", os.Getpid(), testDBSeq.Add(1))
	name := truncateBytes(packagePrefix(baseURL), postgresIdentifierMaxBytes-len(suffix)) + suffix

	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("connecting to base database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	stmt := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s STRATEGY %s",
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{tmpl}.Sanitize(), templateStrategy)
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("creating test database from template: %w", err)
	}

	u := *baseURL
	u.Path = "/" + name
	return &testDB{name: name, url: u.String()}, nil
}

// templateDBName はテンプレートを一度だけ用意し、その名前を返す。
func templateDBName(ctx context.Context, base string) (string, error) {
	templateDB.once.Do(func() {
		templateDB.name, templateDB.err = ensureTemplate(ctx, base)
	})
	return templateDB.name, templateDB.err
}

// dropTestDB はテスト用の DB を強制的に DROP する。プールは呼び出し側が先に閉じている
// （WITH (FORCE) は残った接続を切るので、閉じ忘れがあっても消せる）。
func dropTestDB(ctx context.Context, base, name string) error {
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return fmt.Errorf("connecting to base database: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("dropping %s: %w", name, err)
	}
	return nil
}

// truncateAllTables は public スキーマのユーザーテーブル（goose の管理テーブル
// goose_db_version を除く）をすべて TRUNCATE し、シーケンスも 1 から振り直す。
// River のテーブル（river_job など）もマイグレーションで作られる通常のテーブルなので
// 対象に含まれる。
func truncateAllTables(ctx context.Context, dbURL string) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connecting to test database: %w", err)
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
