package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

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

// recordingEncodePolicyMigrationVersion は 00032_recording_encode_policy.sql の
// goose バージョン番号。ファイル名の連番プレフィックスと一致させる（ずれたら
// このテストがすぐ壊れて気付ける）。
const recordingEncodePolicyMigrationVersion = 32

// TestMigrateUp_RecordingEncodePolicyBackfill は issue #159 の 00032 マイグレーション
// の backfill を検証する。goose で 00029 まで適用した「移設前」のスキーマ
// （recordings.keep_original / encode_profiles が列として存在する）にフィクスチャを
// 直接書き込み、00032 を適用した後の recording_encode_policy の行の有無・値を確認する。
//
// backfill の判定基準は「原本 media_asset（kind='original'）を持つか」であって
// 列の値そのものではない（既定値のままでも原本があれば凍結済みとして row を作る。
// 00032 のコメント参照）。3 ケースで両方向を確認する:
//
//   - A: 原本あり + 列が既定値（'always'/'{}'）→ 行ができ、値は既定値のまま
//   - B: 原本あり + 列が非既定値（'until_encoded'/['h265']）→ 行ができ、値も引き継がれる
//   - C: 原本なし（列は既定値のまま。never-scheduled 等）→ 行はできない
//
// Down も確認する: recording_encode_policy から recordings へ書き戻り、
// 衛星表は消える。
func TestMigrateUp_RecordingEncodePolicyBackfill(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		_ = MigrateReset(context.Background(), dbURL)
	})

	// --- 00029 まで（衛星表が無い「移設前」のスキーマ）を適用 ---
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, recordingEncodePolicyMigrationVersion-1)
		return err
	}); err != nil {
		t.Fatalf("migrating up to version %d: %v", recordingEncodePolicyMigrationVersion-1, err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting pool: %v", err)
	}
	defer pool.Close()

	insertLegacyRecording := func(eventID int32, keepOriginal string, encodeProfiles []string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO recordings (
				source, site, network_id, service_id, event_id, service_name,
				channel_type, channel, title, program_start_at, program_duration_ms,
				status, keep_original, encode_profiles
			) VALUES (
				'manual', 'default', 32736, 1024, $1, 'テスト局',
				'GR', '27', 'backfill-test', now(), 1800000,
				'finished', $2, $3
			) RETURNING id`,
			eventID, keepOriginal, encodeProfiles,
		).Scan(&id); err != nil {
			t.Fatalf("inserting legacy recording: %v", err)
		}
		return id
	}
	insertOriginalAsset := func(recordingID int64, relPath string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
			VALUES ($1, 'original', $2, 1)`, recordingID, relPath); err != nil {
			t.Fatalf("inserting legacy media_asset: %v", err)
		}
	}

	// A: 原本あり + 既定値。
	recA := insertLegacyRecording(1, "always", []string{})
	insertOriginalAsset(recA, "a.m2ts")

	// B: 原本あり + 非既定値。
	recB := insertLegacyRecording(2, "until_encoded", []string{"h265"})
	insertOriginalAsset(recB, "b.m2ts")

	// C: 原本なし（既定値のまま。ingest 未完了 / never-scheduled 相当）。
	recC := insertLegacyRecording(3, "always", []string{})

	// --- 00032 を適用（backfill を含む） ---
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, recordingEncodePolicyMigrationVersion)
		return err
	}); err != nil {
		t.Fatalf("migrating up to version %d: %v", recordingEncodePolicyMigrationVersion, err)
	}

	type policyRow struct {
		KeepOriginal   string
		EncodeProfiles []string
	}
	queryPolicy := func(recordingID int64) (policyRow, bool) {
		t.Helper()
		var row policyRow
		err := pool.QueryRow(ctx,
			"SELECT keep_original, encode_profiles FROM recording_encode_policy WHERE recording_id = $1",
			recordingID,
		).Scan(&row.KeepOriginal, &row.EncodeProfiles)
		if errors.Is(err, pgx.ErrNoRows) {
			return policyRow{}, false
		}
		if err != nil {
			t.Fatalf("querying recording_encode_policy for %d: %v", recordingID, err)
		}
		return row, true
	}

	if row, ok := queryPolicy(recA); !ok {
		t.Errorf("recording A (original present, default values): expected a backfilled row, found none")
	} else if row.KeepOriginal != "always" || len(row.EncodeProfiles) != 0 {
		t.Errorf("recording A backfilled row = %+v, want always/[]", row)
	}

	if row, ok := queryPolicy(recB); !ok {
		t.Errorf("recording B (original present, non-default values): expected a backfilled row, found none")
	} else if row.KeepOriginal != "until_encoded" || !slices.Equal(row.EncodeProfiles, []string{"h265"}) {
		t.Errorf("recording B backfilled row = %+v, want until_encoded/[h265]", row)
	}

	if _, ok := queryPolicy(recC); ok {
		t.Errorf("recording C (no original media_asset): expected no row, but one was backfilled " +
			"(backfill must key off media_assets presence, not column defaults)")
	}

	// recordings から列が落ちていること。
	var colCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'recordings' AND column_name IN ('keep_original', 'encode_profiles')
	`).Scan(&colCount); err != nil {
		t.Fatalf("querying information_schema.columns: %v", err)
	}
	if colCount != 0 {
		t.Errorf("recordings still has keep_original/encode_profiles columns after migration 00032 (count=%d)", colCount)
	}

	// --- Down: recordings へ書き戻り、衛星表が消えること ---
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.DownTo(ctx, recordingEncodePolicyMigrationVersion-1)
		return err
	}); err != nil {
		t.Fatalf("migrating down to version %d: %v", recordingEncodePolicyMigrationVersion-1, err)
	}

	var gotKeepOriginal string
	var gotEncodeProfiles []string
	if err := pool.QueryRow(ctx,
		"SELECT keep_original, encode_profiles FROM recordings WHERE id = $1", recB,
	).Scan(&gotKeepOriginal, &gotEncodeProfiles); err != nil {
		t.Fatalf("querying recordings after down migration: %v", err)
	}
	if gotKeepOriginal != "until_encoded" || !slices.Equal(gotEncodeProfiles, []string{"h265"}) {
		t.Errorf("recordings.keep_original/encode_profiles after down = %q/%v, want until_encoded/[h265]",
			gotKeepOriginal, gotEncodeProfiles)
	}

	var satelliteExists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'recording_encode_policy')",
	).Scan(&satelliteExists); err != nil {
		t.Fatalf("checking recording_encode_policy existence: %v", err)
	}
	if satelliteExists {
		t.Errorf("recording_encode_policy table still exists after down migration")
	}
}

// purgeRequestMigrationVersion は recordings.purge_after を
// recording_purge_requests 衛星表へ移すマイグレーションの goose バージョン番号。
// ファイル名の連番プレフィックスと一致させる。
const purgeRequestMigrationVersion = 39

// TestMigrateUp_PurgeRequestBackfill は上記マイグレーションのデータ引き継ぎを
// 検証する。Up の backfill INSERT と Down の書き戻し UPDATE はどちらを消しても
// internal/db 以下の他のテストは全部 pass する（スキーマの形だけを見ているので、
// 移設前の行が持っていた要求が消えても誰も気付かない）。
//
// goose で 1 つ前まで適用した「移設前」のスキーマ（recordings.purge_after が
// timestamptz 列として存在する）にフィクスチャを直接書き込み、移設後の
// recording_purge_requests の中身を確認する。Down で purge_after への書き戻りも
// 確認する。
func TestMigrateUp_PurgeRequestBackfill(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		_ = MigrateReset(context.Background(), dbURL)
	})

	// --- 00038 まで（purge_after が列として存在する「移設前」のスキーマ）を適用 ---
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, purgeRequestMigrationVersion-1)
		return err
	}); err != nil {
		t.Fatalf("migrating up to version %d: %v", purgeRequestMigrationVersion-1, err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting pool: %v", err)
	}
	defer pool.Close()

	insertLegacyRecording := func(eventID int32, purgeAfter *time.Time) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO recordings (
				source, site, network_id, service_id, event_id, service_name,
				channel_type, channel, title, program_start_at, program_duration_ms,
				status, purge_after
			) VALUES (
				'manual', 'default', 32736, 1024, $1, 'テスト局',
				'GR', '27', 'purge-requested-test', now(), 1800000,
				'finished', $2
			) RETURNING id`,
			eventID, purgeAfter,
		).Scan(&id); err != nil {
			t.Fatalf("inserting legacy recording: %v", err)
		}
		return id
	}

	// timestamptz はマイクロ秒精度なので、往復で Equal を比較する値は
	// あらかじめ切り捨てておく（切り捨てないと round-trip で必ず落ちる）。
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	// A: 旧列に印が付いていた行（過去の purge_after）。
	recA := insertLegacyRecording(1, &past)
	// B: 印が付いていない行（purge_after IS NULL）。
	recB := insertLegacyRecording(2, nil)

	// --- 00039 を適用（backfill を含む） ---
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, purgeRequestMigrationVersion)
		return err
	}); err != nil {
		t.Fatalf("migrating up to version %d: %v", purgeRequestMigrationVersion, err)
	}

	queryPurgeRequestedAt := func(recordingID int64) *time.Time {
		t.Helper()
		var got *time.Time
		if err := pool.QueryRow(ctx,
			"SELECT requested_at FROM recording_purge_requests WHERE recording_id = $1", recordingID,
		).Scan(&got); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			t.Fatalf("querying recording_purge_requests for %d: %v", recordingID, err)
		}
		return got
	}

	// A: 行ができ、旧列の時刻がそのまま requested_at に入る。
	if got := queryPurgeRequestedAt(recA); got == nil {
		t.Errorf("recording A (purge_after set): no recording_purge_requests row, want one")
	} else if !got.Equal(past) {
		t.Errorf("recording A requested_at = %v, want %v (旧列の時刻をそのまま移す)", got, past)
	}
	// B: 行はできない（「要求していない」を表す行を作らない）。
	if got := queryPurgeRequestedAt(recB); got != nil {
		t.Errorf("recording B (purge_after NULL): recording_purge_requests row requested_at = %v, want no row", got)
	}

	// recordings から purge_after 列が落ちていること。
	var colCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'recordings' AND column_name = 'purge_after'
	`).Scan(&colCount); err != nil {
		t.Fatalf("querying information_schema.columns: %v", err)
	}
	if colCount != 0 {
		t.Errorf("recordings still has purge_after column after migration %d (count=%d)",
			purgeRequestMigrationVersion, colCount)
	}

	// --- Down: purge_after へ書き戻ること ---
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.DownTo(ctx, purgeRequestMigrationVersion-1)
		return err
	}); err != nil {
		t.Fatalf("migrating down to version %d: %v", purgeRequestMigrationVersion-1, err)
	}

	var gotPurgeAfterA, gotPurgeAfterB *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT purge_after FROM recordings WHERE id = $1", recA,
	).Scan(&gotPurgeAfterA); err != nil {
		t.Fatalf("querying recordings after down migration (A): %v", err)
	}
	if gotPurgeAfterA == nil {
		t.Errorf("recording A purge_after after down = nil, want non-NULL (印が立っていた行は復元されるべき)")
	}

	if err := pool.QueryRow(ctx,
		"SELECT purge_after FROM recordings WHERE id = $1", recB,
	).Scan(&gotPurgeAfterB); err != nil {
		t.Fatalf("querying recordings after down migration (B): %v", err)
	}
	if gotPurgeAfterB != nil {
		t.Errorf("recording B purge_after after down = %v, want NULL", gotPurgeAfterB)
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

	pool, err := NewPool(ctx, cfg, nil)
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
	_, err := NewPool(ctx, cfg, nil)
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

	_, err := NewPool(ctx, cfg, []string{"worker"})
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
		pool, err := NewPool(ctx, cfg, []string{"api"})
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
		pool, err := NewPool(ctx, cfg, []string{"worker"})
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
