package api

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

const shelfBenchmarkDatabaseURL = "ROKUBAN_BENCH_DATABASE_URL"

// TestListRecordingShelves_PlanBenchmark は棚の母集団を広げる前に、同じ PostgreSQL
// と pgx の prepared statement 経路で候補の形を測るハーネスである。
//
// 実データを破壊しないよう、専用に用意した DB の URL を
// ROKUBAN_BENCH_DATABASE_URL に渡したときだけ実行する。73,000 行を次の比率で作る:
//
//   - 60,000 件（82.2%）: 生きている finished + active original
//   - 5,000 件（6.8%）: 生きている finished + active encoded（original なし）
//   - 3,000 件（4.1%）: 録画中、media_asset なし
//   - 2,000 件（2.7%）: ingest 待ち、media_asset なし
//   - 1,000 件（1.4%）: failed、media_asset なし
//   - 1,000 件（1.4%）: ごみ箱、active original あり
//   - 1,000 件（1.4%）: superseded、active original あり
//
// タイトルは 141 個の自動キーへ均等に分け、分類ルールを 50 本置く。ルールの値は
// 自動キーと同じなので、recording_series の JOIN と評価結果を含むプランを測れる。
// 現行形は sqlc の ListRecordingShelves、候補形は生きている録画を母集団にして
// playable_assets を LEFT JOIN し、見られる件数と latestStartAt を別列で返す形である。
// 各形を同じ接続で 10 回実行し、中央値を t.Logf に出す。pgx の prepared statement が
// 6 回目以降に generic plan へ切り替わるため、単発 psql の値を使わない。
func TestListRecordingShelves_PlanBenchmark(t *testing.T) {
	benchURL := os.Getenv(shelfBenchmarkDatabaseURL)
	if benchURL == "" {
		t.Skip(shelfBenchmarkDatabaseURL + " not set")
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(benchURL)
	if err != nil {
		t.Fatalf("parsing %s: %v", shelfBenchmarkDatabaseURL, err)
	}
	// 全 10 回を同じ backend connection に通し、pgx の prepared statement の
	// custom → generic plan の切替を測定へ含める。
	cfg.MinConns = 1
	cfg.MaxConns = 1
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting benchmark database: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring benchmark connection: %v", err)
	}
	defer conn.Release()
	seedShelfBenchmark(t, conn.Conn())

	queries := sqlcgen.New(conn.Conn())
	currentMedian := measureShelfShape(t, "(a) current ListRecordingShelves", func() error {
		rows, err := queries.ListRecordingShelves(ctx)
		if err != nil {
			return err
		}
		if len(rows) != 141 {
			return fmt.Errorf("current shelf count = %d, want 141", len(rows))
		}
		return nil
	})

	unmaterializedMedian := measureShelfShape(t, "(a') current shape without playable MATERIALIZED", func() error {
		count, err := queryCurrentUnmaterialized(ctx, conn.Conn())
		if err != nil {
			return err
		}
		if count != 141 {
			return fmt.Errorf("unmaterialized shelf count = %d, want 141", count)
		}
		return nil
	})

	expandedMedian := measureShelfShape(t, "(b) live recordings + playable count", func() error {
		count, _, _, err := queryExpanded(ctx, conn.Conn(), expandedShelfQuery)
		if err != nil {
			return err
		}
		if count != 141 {
			return fmt.Errorf("expanded shelf count = %d, want 141", count)
		}
		return nil
	})

	// (b) が予算を越えた場合に選べる形も同じハーネスで測る。常に出しておくと、
	// 測定者が結果を見てから SQL を書き換える必要がなく、形の比較を再現できる。
	expandedMaterializedMedian := measureShelfShape(t, "(b') live CTE MATERIALIZED", func() error {
		count, _, _, err := queryExpanded(ctx, conn.Conn(), expandedMaterializedShelfQuery)
		if err != nil {
			return err
		}
		if count != 141 {
			return fmt.Errorf("materialized expanded shelf count = %d, want 141", count)
		}
		return nil
	})

	_, playable, live, err := queryExpanded(ctx, conn.Conn(), expandedMaterializedShelfQuery)
	if err != nil {
		t.Fatalf("checking expanded result: %v", err)
	}
	if playable != 65_000 || live != 71_000 {
		t.Fatalf("expanded totals = playable %d / live %d, want 65000 / 71000", playable, live)
	}

	t.Logf("shelf benchmark medians: current=%s, unmaterialized=%s, expanded=%s, expanded_materialized=%s; expanded budget=200ms; absolute comparison to the original 141ms environment is not established here", currentMedian, unmaterializedMedian, expandedMedian, expandedMaterializedMedian)
	if unmaterializedMedian <= currentMedian {
		t.Errorf("removing playable MATERIALIZED was not slower: current=%s, unmaterialized=%s", currentMedian, unmaterializedMedian)
	}
}

func measureShelfShape(t *testing.T, label string, run func() error) time.Duration {
	t.Helper()
	samples := make([]time.Duration, 10)
	for i := range samples {
		started := time.Now()
		if err := run(); err != nil {
			t.Fatalf("%s run %d: %v", label, i+1, err)
		}
		samples[i] = time.Since(started)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return (samples[4] + samples[5]) / 2
}

func seedShelfBenchmark(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
TRUNCATE recordings, media_assets, label_rules, label_rule_hits RESTART IDENTITY CASCADE;

INSERT INTO recordings (
  source, site, network_id, service_id, event_id, service_name, channel_type, channel,
  title, program_start_at, program_duration_ms, status, deleted_at, superseded_at
)
SELECT
  'manual', 'default', 32678, 5168, i, 'benchmark', 'GR', '27',
  format('シリーズ%s 第%s回', lpad(((i - 1) % 141)::text, 3, '0'), i),
  timestamptz '2020-01-01 00:00:00+00' + i * interval '1 minute',
  1800000,
  CASE
    WHEN i <= 65000 THEN 'finished'
    WHEN i <= 68000 THEN 'recording'
    WHEN i <= 70000 THEN 'finished'
    ELSE 'failed'
  END,
  CASE WHEN i BETWEEN 71001 AND 72000 THEN now() ELSE NULL END,
  CASE WHEN i BETWEEN 72001 AND 73000 THEN now() ELSE NULL END
FROM generate_series(1, 73000) AS s(i);

INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state)
SELECT
  i,
  CASE WHEN i BETWEEN 60001 AND 65000 THEN 'encoded' ELSE 'original' END,
  CASE WHEN i BETWEEN 60001 AND 65000 THEN 'bench' ELSE NULL END,
  format('bench/%s', i),
  1,
  'active'
FROM generate_series(1, 73000) AS s(i)
WHERE i <= 65000 OR i BETWEEN 71001 AND 73000;

INSERT INTO label_rules (key, value, keyword, priority)
SELECT
  'series',
  format('シリーズ%s', lpad(i::text, 3, '0')),
  format('シリーズ%s', lpad(i::text, 3, '0')),
  50 - i
FROM generate_series(0, 49) AS s(i);
	`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("seeding shelf benchmark: %v", err)
	}

	if _, err := sqlcgen.New(conn).ApplyLabelRuleReevaluation(ctx); err != nil {
		t.Fatalf("evaluating benchmark label rules: %v", err)
	}
	if _, err := conn.Exec(ctx, `
ANALYZE recordings;
ANALYZE media_assets;
ANALYZE label_rules;
ANALYZE label_rule_hits;
	`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("analyzing shelf benchmark tables: %v", err)
	}
}

func queryCurrentUnmaterialized(ctx context.Context, conn *pgx.Conn) (int, error) {
	rows, err := conn.Query(ctx, currentUnmaterializedShelfQuery)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	return scanCurrentShelfRows(rows)
}

func scanCurrentShelfRows(rows pgx.Rows) (int, error) {
	count := 0
	for rows.Next() {
		var value pgtype.Text
		var title string
		var recordingCount, representativeID int64
		if err := rows.Scan(&value, &title, &recordingCount, &representativeID); err != nil {
			return 0, err
		}
		count++
	}
	return count, rows.Err()
}

func queryExpanded(ctx context.Context, conn *pgx.Conn, query string) (shelves int, playable int64, live int64, err error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var value pgtype.Text
		var title string
		var playableCount, recordingCount int64
		var latestStartAt time.Time
		var representativeID int64
		if err := rows.Scan(&value, &title, &playableCount, &recordingCount, &latestStartAt, &representativeID); err != nil {
			return 0, 0, 0, err
		}
		shelves++
		playable += playableCount
		live += recordingCount
	}
	return shelves, playable, live, rows.Err()
}

const currentUnmaterializedShelfQuery = `
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
playable AS (
    SELECT r.id,
           r.title,
           r.program_start_at,
           rs.value
    FROM recordings r
    JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT p.value,
       (array_agg(p.title ORDER BY p.program_start_at DESC, p.id DESC))[1]::text AS title,
       count(*) AS recording_count,
       (array_agg(p.id ORDER BY p.program_start_at DESC, p.id DESC))[1]::bigint AS representative_id
FROM playable p
GROUP BY p.value
ORDER BY recording_count DESC, p.value ASC NULLS LAST
`

const expandedShelfQuery = `
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
live AS (
    SELECT r.id,
           r.title,
           r.program_start_at,
           rs.value,
           pa.recording_id AS playable_recording_id
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       count(*) AS recording_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`

const expandedMaterializedShelfQuery = `
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
live AS MATERIALIZED (
    SELECT r.id,
           r.title,
           r.program_start_at,
           rs.value,
           pa.recording_id AS playable_recording_id
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       count(*) AS recording_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`
