package api

import (
	"context"
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

// TestListRecordingShelves_PlanBenchmark は棚のクエリの形を、同じ PostgreSQL と pgx の
// prepared statement 経路で測るハーネスである。
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
//
// 測る形は 4 つである。
//
//   - (a) 本番: sqlc の ListRecordingShelves。生きている録画を母集団にして playable_assets を
//     LEFT JOIN し、見られる件数（count FILTER）と latestStartAt（max）を同じ集計から返す形
//   - (o) 旧形: 再生できる録画だけを INNER JOIN した母集団（playable を MATERIALIZED）。
//     母集団が (a) と違うので比較用のリテラルとして持つ
//   - (o') (o) から playable の MATERIALIZED を外した形。同じ母集団どうしの比較
//   - (b') (a) の live を MATERIALIZED にした形
//
// 各形を同じ接続で、形を交互に回すラウンド 10 回ずつ実行し（実行順の偏りを消す）、
// 中央値を t.Logf に出す。(a) との比も出すが、判定には使わない。
//
// 判定しているのは結果の一致だけである。棚ごとに (a) の playable_count と (o) の recording_count
// （母集団が違うのでこの対応で見る）、(o') と (o) の全列、(b') と (a) の全列が一致し、
// (a) の latest_start_at は別クエリで求めた「その棚の生きている録画の program_start_at の最大値」と
// 一致する。(a) の本番 SQL の max や FILTER を壊すとここで落ちる。
//
// 既知の 617 ms（playable の MATERIALIZED を外すと数倍遅い）は、現スキーマ・この合成
// seed では再現しない（Apple M3 Max・PostgreSQL 16.2 で (o') / (o) は 0.92〜0.96）。
// 棚サイズの偏り・複数の自動キーを 1 棚に併合する分類ルール・統計なしの状態でも再現せず、
// 617 ms の再現条件は未検証である。したがって (o') が遅いことはアサートしない。
//
// 渡された DB はマイグレーション済みであることを前提とし、seed の冒頭で
// recordings / media_assets / label_rules / label_rule_hits を無条件に TRUNCATE する
// （空の DB では relation does not exist で落ちる）。必ず専用の DB を渡す。
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
	// 全ラウンドを同じ backend connection に通し、pgx の prepared statement の
	// キャッシュを使う経路（アプリと同じ）で測る。
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
	shapes := []shelfShape{
		{name: "(a) production ListRecordingShelves", run: func() (map[string]shelfResult, error) {
			rows, err := queries.ListRecordingShelves(ctx)
			if err != nil {
				return nil, err
			}
			out := make(map[string]shelfResult, len(rows))
			for _, r := range rows {
				out[shelfKey(r.Value)] = shelfResult{
					title: r.Title, recording: r.RecordingCount, playable: r.PlayableCount,
					latest: r.LatestStartAt, representative: r.RepresentativeID,
				}
			}
			return out, nil
		}},
		{name: "(o) previous playable-only shape", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), previousShelfQuery, false)
		}},
		{name: "(o') previous shape without playable MATERIALIZED", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), previousUnmaterializedShelfQuery, false)
		}},
		{name: "(b') production shape with live MATERIALIZED", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), liveMaterializedShelfQuery, true)
		}},
	}

	const rounds = 10
	samples := make([][]time.Duration, len(shapes))
	results := make([]map[string]shelfResult, len(shapes))
	for round := 0; round < rounds; round++ {
		for i, shape := range shapes {
			started := time.Now()
			got, err := shape.run()
			if err != nil {
				t.Fatalf("%s round %d: %v", shape.name, round+1, err)
			}
			samples[i] = append(samples[i], time.Since(started))
			results[i] = got
		}
	}
	medians := make([]time.Duration, len(shapes))
	for i, shape := range shapes {
		if len(results[i]) != 141 {
			t.Fatalf("%s shelf count = %d, want 141", shape.name, len(results[i]))
		}
		medians[i] = median(samples[i])
		t.Logf("%s: median %s", shape.name, medians[i])
	}

	production, previous, previousUnmaterialized, liveMaterialized := results[0], results[1], results[2], results[3]
	wantLatest, err := queryExpectedLatest(ctx, conn.Conn())
	if err != nil {
		t.Fatalf("computing expected latest_start_at: %v", err)
	}
	var playable, live int64
	for key, prod := range production {
		prev := previous[key]
		if previousUnmaterialized[key] != prev {
			t.Errorf("shelf %q: (o') %+v != (o) %+v", key, previousUnmaterialized[key], prev)
		}
		if prod.playable != prev.recording {
			t.Errorf("shelf %q: (a) playable_count %d != (o) recording_count %d", key, prod.playable, prev.recording)
		}
		if want, ok := wantLatest[key]; !ok || !prod.latest.Equal(want) {
			t.Errorf("shelf %q: (a) latest_start_at %v != expected %v", key, prod.latest, want)
		}
		if liveMaterialized[key] != prod {
			t.Errorf("shelf %q: (b') %+v != (a) %+v", key, liveMaterialized[key], prod)
		}
		playable += prod.playable
		live += prod.recording
	}
	if playable != 65_000 || live != 71_000 {
		t.Errorf("production totals = playable %d / live %d, want 65000 / 71000", playable, live)
	}

	t.Logf("ratios to (a): (o)=%.2f, (o')=%.2f, (b')=%.2f; budget=200ms; absolute comparison to the original 141ms environment is not established here",
		float64(medians[1])/float64(medians[0]), float64(medians[2])/float64(medians[0]), float64(medians[3])/float64(medians[0]))
}

type shelfShape struct {
	name string
	run  func() (map[string]shelfResult, error)
}

// shelfResult は 1 棚ぶんの結果で、形の間で全列を比較する。旧形（playable のみ）は
// recording だけを持ち、playable / latest は本番形だけが埋める。
type shelfResult struct {
	title          string
	recording      int64
	playable       int64
	latest         time.Time
	representative int64
}

// queryExpectedLatest は棚ごとの latest_start_at の期待値を、測る SQL とは別のクエリで求める。
// 生きている録画（deleted_at / superseded_at が NULL）の program_start_at の最大値である。
func queryExpectedLatest(ctx context.Context, conn *pgx.Conn) (map[string]time.Time, error) {
	rows, err := conn.Query(ctx, `
SELECT rs.value, max(r.program_start_at)
FROM recordings r
JOIN recording_series rs ON rs.recording_id = r.id
WHERE r.deleted_at IS NULL AND r.superseded_at IS NULL
GROUP BY rs.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var v *string
		var latest time.Time
		if err := rows.Scan(&v, &latest); err != nil {
			return nil, err
		}
		out[shelfKey(v)] = latest
	}
	return out, rows.Err()
}

func shelfKey(v *string) string {
	if v == nil {
		return "<NULL>"
	}
	return *v
}

func median(samples []time.Duration) time.Duration {
	s := append([]time.Duration(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// queryShelves は棚のクエリを実行して棚ごとの結果を返す。expanded は playable_count と
// latest_start_at の列を持つ形（本番形の派生）を読む。
func queryShelves(ctx context.Context, conn *pgx.Conn, query string, expanded bool) (map[string]shelfResult, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]shelfResult{}
	for rows.Next() {
		var value pgtype.Text
		var r shelfResult
		if expanded {
			err = rows.Scan(&value, &r.title, &r.playable, &r.recording, &r.latest, &r.representative)
		} else {
			err = rows.Scan(&value, &r.title, &r.recording, &r.representative)
		}
		if err != nil {
			return nil, err
		}
		var v *string
		if value.Valid {
			v = &value.String
		}
		out[shelfKey(v)] = r
	}
	return out, rows.Err()
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
	// VACUUM は暗黙のトランザクションになる複数文の送信では実行できないので 1 文ずつ送る。
	for _, table := range []string{"recordings", "media_assets", "label_rules", "label_rule_hits"} {
		if _, err := conn.Exec(ctx, "VACUUM ANALYZE "+table); err != nil {
			t.Fatalf("vacuum-analyzing %s: %v", table, err)
		}
	}
}

const previousShelfQuery = `
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
playable AS MATERIALIZED (
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

const previousUnmaterializedShelfQuery = `
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

const liveMaterializedShelfQuery = `
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
