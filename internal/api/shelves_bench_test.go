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
// タイトルは 141 個の自動キーへ均等に分け、分類ルールを 50 本置く。各放送イベントは
// 2 拠点にまたがる 2 行として作り、10 行に 1 行の割合で視聴済み印を付ける。
//
// 測る形は、本番の分割クエリ、統合集計、棚集計だけの基準、playable_assets を
// MATERIALIZED にした比較形、逐次分割、旧形 2 種、live を MATERIALIZED にした形である。
//
//   - (a) 本番: 棚の sqlc クエリと未視聴件数の sqlc クエリを別接続で並列実行する
//   - (c) 棚の集計に未視聴イベント数を統合する形
//   - (a0) 棚の一覧集計だけの所要時間
//   - (c-sequential) 同じ 2 クエリを単一接続で逐次実行する
//   - (o) 旧形: 再生できる録画だけを INNER JOIN した母集団（playable を MATERIALIZED）。
//     母集団が (a) と違うので比較用のリテラルとして持つ
//   - (o') (o) から playable の MATERIALIZED を外した形。同じ母集団どうしの比較
//   - (b') 棚クエリの live を MATERIALIZED にした形
//
// 各形を同じ接続で、形を交互に回すラウンド 10 回ずつ実行し（実行順の偏りを消す）、
// 中央値を t.Logf に出す。(a) との比も出すが、判定には使わない。
//
// 判定しているのは結果の一致だけである。棚ごとに (a)/(c)/(c-sequential)/(b') の全列、
// (a0) の未視聴数以外、(o') と (o) の全列が一致し、(a) の latest_start_at と
// unwatched_count は別クエリで求めた期待値と一致させる。
//
// 既知の 617 ms（旧 playable CTE の MATERIALIZED を外すと数倍遅い）は、現スキーマ・この合成
// seed では再現しない（Apple M3 Max・PostgreSQL 16.2 で (o') / (o) は 0.94〜0.95）。
// 棚サイズの偏り・複数の自動キーを 1 棚に併合する分類ルール・統計なしの状態でも再現せず、
// 617 ms の再現条件は未検証である。したがって (o') が遅いことはアサートしない。
// (o)/(o') は未視聴集計を含まないため、本番形と同機能の性能比較には使わない。
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
	parallelCountConn, err := pgx.Connect(ctx, benchURL)
	if err != nil {
		t.Fatalf("opening second connection for parallel shelf benchmark: %v", err)
	}
	defer func() { _ = parallelCountConn.Close(context.Background()) }()

	queries := sqlcgen.New(conn.Conn())
	parallelCountQueries := sqlcgen.New(parallelCountConn)
	shapes := []shelfShape{
		{name: "(a) production shelf + parallel unwatched queries", run: func() (map[string]shelfResult, error) {
			return queryShelfAndSeparateUnwatchedSQLC(ctx, queries, parallelCountQueries)
		}},
		{name: "(c) integrated unwatched aggregation", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), integratedUnwatchedShelfQuery, true)
		}},
		{name: "(a0) shelf aggregation without unwatched count", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), productionWithoutUnwatchedQuery, true)
		}},
		{name: "(a0-materialized) shelf with MATERIALIZED playable_assets", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), shelfMaterializedAssetsQuery, true)
		}},
		{name: "(c-sequential) shelf + separate query sequentially", run: func() (map[string]shelfResult, error) {
			return queryShelfAndSeparateUnwatched(ctx, conn.Conn())
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

	production, integrated, productionWithoutUnwatched := results[0], results[1], results[2]
	shelfWithMaterializedAssets, shelfAndSeparateUnwatched := results[3], results[4]
	previous, previousUnmaterialized, liveMaterialized := results[5], results[6], results[7]
	wantLatest, err := queryExpectedLatest(ctx, conn.Conn())
	if err != nil {
		t.Fatalf("computing expected latest_start_at: %v", err)
	}
	wantUnwatched, err := queryExpectedUnwatchedEvents(ctx, conn.Conn())
	if err != nil {
		t.Fatalf("computing expected unwatched event count: %v", err)
	}
	var playable, live int64
	for key, prod := range production {
		if integrated[key] != prod {
			t.Errorf("shelf %q: integrated candidate %+v != production %+v", key, integrated[key], prod)
		}
		if shelfAndSeparateUnwatched[key] != prod {
			t.Errorf("shelf %q: separate-query candidate %+v != production %+v", key, shelfAndSeparateUnwatched[key], prod)
		}
		baseShape := prod
		baseShape.unwatched = 0
		if productionWithoutUnwatched[key] != baseShape {
			t.Errorf("shelf %q: (a0) %+v != (a) without unwatched_count %+v", key, productionWithoutUnwatched[key], baseShape)
		}
		if shelfWithMaterializedAssets[key] != baseShape {
			t.Errorf("shelf %q: (a0-materialized) %+v != baseline %+v", key, shelfWithMaterializedAssets[key], baseShape)
		}
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
		if want, ok := wantUnwatched[key]; !ok || prod.unwatched != want {
			t.Errorf("shelf %q: (a) unwatched_count %d != expected %d", key, prod.unwatched, want)
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

	t.Logf("ratios to (a): (c)=%.2f, (a0)=%.2f, (a0-materialized)=%.2f, (c-sequential)=%.2f, (o)=%.2f, (o')=%.2f, (b')=%.2f; budget=200ms",
		float64(medians[1])/float64(medians[0]), float64(medians[2])/float64(medians[0]), float64(medians[3])/float64(medians[0]), float64(medians[4])/float64(medians[0]), float64(medians[5])/float64(medians[0]), float64(medians[6])/float64(medians[0]), float64(medians[7])/float64(medians[0]))

	measureReservationSeriesCosts(t, ctx, conn.Conn(), queries)
}

func measureReservationSeriesCosts(t *testing.T, ctx context.Context, conn *pgx.Conn, queries *sqlcgen.Queries) {
	t.Helper()
	if _, err := conn.Exec(ctx, `TRUNCATE reservations, program_snapshots, epg_programs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("clearing reservation-series benchmark tables: %v", err)
	}
	seedReservationSeriesBenchmarkRows(t, ctx, conn, 1, 500)
	analyzeReservationSeriesBenchmarkTables(t, ctx, conn)
	measureListReservationsFull(t, ctx, queries, 500)

	seedReservationSeriesBenchmarkRows(t, ctx, conn, 501, 2000)
	analyzeReservationSeriesBenchmarkTables(t, ctx, conn)
	measureListReservationsFull(t, ctx, queries, 2000)
}

func seedReservationSeriesBenchmarkRows(t *testing.T, ctx context.Context, conn *pgx.Conn, first, last int) {
	t.Helper()
	const insertEpg = `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at,
  is_free, name, description
)
SELECT 'default', 800000000 + i, 32678, 5168, i,
       timestamptz '2020-01-01 00:00:00+00' + i * interval '1 minute', 1800000,
       timestamptz '2020-01-01 00:00:00+00' + i * interval '1 minute' + interval '30 minutes',
       true, format('シリーズ%s 第%s回', lpad((i % 141)::text, 3, '0'), i), ''
FROM generate_series($1::integer, $2::integer) AS s(i)`
	if _, err := conn.Exec(ctx, insertEpg, first, last); err != nil {
		t.Fatalf("seeding EPG programs for reservation-series benchmark: %v", err)
	}
	const insertSnapshots = `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms, network_id, service_id,
  channel_type, channel, event_id, service_name
)
SELECT 'default', 800000000 + i,
       format('シリーズ%s 第%s回', lpad((i % 141)::text, 3, '0'), i),
       timestamptz '2020-01-01 00:00:00+00' + i * interval '1 minute', 1800000,
       32678, 5168, 'GR', '27', i, 'benchmark'
FROM generate_series($1::integer, $2::integer) AS s(i)`
	if _, err := conn.Exec(ctx, insertSnapshots, first, last); err != nil {
		t.Fatalf("seeding program snapshots for reservation-series benchmark: %v", err)
	}
	const insertReservations = `
INSERT INTO reservations (site, program_id, base)
SELECT 'default', 800000000 + i, '{}'::jsonb
FROM generate_series($1::integer, $2::integer) AS s(i)`
	if _, err := conn.Exec(ctx, insertReservations, first, last); err != nil {
		t.Fatalf("seeding reservations for reservation-series benchmark: %v", err)
	}
}

func analyzeReservationSeriesBenchmarkTables(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	for _, table := range []string{"epg_programs", "program_snapshots", "reservations"} {
		if _, err := conn.Exec(ctx, "ANALYZE "+table); err != nil {
			t.Fatalf("analyzing %s for reservation-series benchmark: %v", table, err)
		}
	}
}

func measureListReservationsFull(t *testing.T, ctx context.Context, queries *sqlcgen.Queries, wantRows int) {
	t.Helper()
	const rounds = 10
	samples := make([]time.Duration, 0, rounds)
	for round := 0; round < rounds; round++ {
		started := time.Now()
		rows, err := queries.ListReservationsFull(ctx)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("ListReservationsFull round %d at %d rows: %v", round+1, wantRows, err)
		}
		if len(rows) != wantRows {
			t.Fatalf("ListReservationsFull row count = %d, want %d", len(rows), wantRows)
		}
		if len(rows) > 0 && rows[0].Series == nil {
			t.Fatalf("ListReservationsFull row %d has null series, want a derived series", rows[0].ProgramSnapshot.ProgramID)
		}
		samples = append(samples, elapsed)
	}
	t.Logf("(d) ListReservationsFull with %d reservations: median %s", wantRows, median(samples))
}

type shelfShape struct {
	name string
	run  func() (map[string]shelfResult, error)
}

// shelfResult は 1 棚ぶんの結果で、形の間で全列を比較する。旧形（playable のみ）は
// recording だけを持ち、playable / unwatched / latest は本番形だけが埋める。
type shelfResult struct {
	title          string
	recording      int64
	playable       int64
	unwatched      int64
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

func queryExpectedUnwatchedEvents(ctx context.Context, conn *pgx.Conn) (map[string]int64, error) {
	rows, err := conn.Query(ctx, `
SELECT rs.value,
       count(DISTINCT (r.network_id, r.service_id, r.program_start_at))
FROM recordings r
JOIN recording_series rs ON rs.recording_id = r.id
WHERE r.deleted_at IS NULL
  AND r.superseded_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM recording_watched w
      JOIN recordings watched_recording ON watched_recording.id = w.recording_id
      WHERE watched_recording.network_id = r.network_id
        AND watched_recording.service_id = r.service_id
        AND watched_recording.program_start_at = r.program_start_at
  )
GROUP BY rs.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var v *string
		var count int64
		if err := rows.Scan(&v, &count); err != nil {
			return nil, err
		}
		out[shelfKey(v)] = count
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

// queryShelves は棚のクエリを実行して棚ごとの結果を返す。expanded は本番形と同じ列を持つ
// (b') の結果を読む。
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
			err = rows.Scan(&value, &r.title, &r.playable, &r.recording, &r.unwatched, &r.latest, &r.representative)
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

func queryShelfAndSeparateUnwatched(ctx context.Context, conn *pgx.Conn) (map[string]shelfResult, error) {
	shelves, err := queryShelves(ctx, conn, productionWithoutUnwatchedQuery, true)
	if err != nil {
		return nil, err
	}
	counts, err := queryUnwatchedCounts(ctx, conn)
	if err != nil {
		return nil, err
	}
	return mergeUnwatchedCounts(shelves, counts)
}

func queryShelfAndSeparateUnwatchedSQLC(ctx context.Context, shelfQueries, countQueries *sqlcgen.Queries) (map[string]shelfResult, error) {
	type shelfResultValue struct {
		rows map[string]shelfResult
		err  error
	}
	type countResultValue struct {
		rows []sqlcgen.ListRecordingShelvesUnwatchedCountRow
		err  error
	}
	shelfCh := make(chan shelfResultValue, 1)
	countCh := make(chan countResultValue, 1)
	go func() {
		rows, err := shelfQueries.ListRecordingShelves(ctx)
		if err != nil {
			shelfCh <- shelfResultValue{err: err}
			return
		}
		out := make(map[string]shelfResult, len(rows))
		for _, row := range rows {
			out[shelfKey(row.Value)] = shelfResult{
				title: row.Title, recording: row.RecordingCount, playable: row.PlayableCount,
				latest: row.LatestStartAt, representative: row.RepresentativeID,
			}
		}
		shelfCh <- shelfResultValue{rows: out}
	}()
	go func() {
		rows, err := countQueries.ListRecordingShelvesUnwatchedCount(ctx)
		countCh <- countResultValue{rows: rows, err: err}
	}()
	shelves, counts := <-shelfCh, <-countCh
	if shelves.err != nil {
		return nil, shelves.err
	}
	if counts.err != nil {
		return nil, counts.err
	}
	for _, row := range counts.rows {
		key := shelfKey(row.Value)
		shelf, ok := shelves.rows[key]
		if !ok {
			return nil, fmt.Errorf("unwatched query returned unknown shelf %q", key)
		}
		shelf.unwatched = row.UnwatchedCount
		shelves.rows[key] = shelf
	}
	return shelves.rows, nil
}

func queryUnwatchedCounts(ctx context.Context, conn *pgx.Conn) (map[string]int64, error) {
	rows, err := conn.Query(ctx, separateUnwatchedCountQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var value pgtype.Text
		var count int64
		if err := rows.Scan(&value, &count); err != nil {
			return nil, err
		}
		var v *string
		if value.Valid {
			v = &value.String
		}
		counts[shelfKey(v)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func mergeUnwatchedCounts(shelves map[string]shelfResult, counts map[string]int64) (map[string]shelfResult, error) {
	for key, count := range counts {
		shelf, ok := shelves[key]
		if !ok {
			return nil, fmt.Errorf("unwatched query returned unknown shelf %q", key)
		}
		shelf.unwatched = count
		shelves[key] = shelf
	}
	return shelves, nil
}

func seedShelfBenchmark(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
TRUNCATE recording_watched, recordings, media_assets, label_rules, label_rule_hits RESTART IDENTITY CASCADE;

INSERT INTO recordings (
  source, site, network_id, service_id, event_id, service_name, channel_type, channel,
  title, program_start_at, program_duration_ms, status, deleted_at, superseded_at
)
SELECT
  'manual', CASE WHEN i % 2 = 0 THEN 'osaka' ELSE 'tokyo' END,
  32678, 5168, (i + 1) / 2, 'benchmark', 'GR', '27',
  format('シリーズ%s 第%s回', lpad((((i - 1) / 2) % 141)::text, 3, '0'), (i + 1) / 2),
  timestamptz '2020-01-01 00:00:00+00' + ((i - 1) / 2) * interval '1 minute',
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

INSERT INTO recording_watched (recording_id)
SELECT id FROM recordings WHERE id % 10 = 0;

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

const separateUnwatchedCountQuery = `
WITH live_events AS (
    SELECT DISTINCT rs.value, r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT le.value, count(*)::bigint
FROM live_events le
WHERE NOT EXISTS (
    SELECT 1
    FROM recordings watched_recording
    JOIN recording_watched w ON w.recording_id = watched_recording.id
    WHERE watched_recording.network_id = le.network_id
      AND watched_recording.service_id = le.service_id
      AND watched_recording.program_start_at = le.program_start_at
)
GROUP BY le.value
`

const integratedUnwatchedShelfQuery = `
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
watched_events AS (
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recording_watched w
    JOIN recordings r ON r.id = w.recording_id
),
live AS (
    SELECT r.id,
           r.title,
           r.program_start_at,
           r.network_id,
           r.service_id,
           rs.value,
           pa.recording_id AS playable_recording_id,
			we.network_id IS NULL AS unwatched
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    LEFT JOIN watched_events we
      ON we.network_id = r.network_id
     AND we.service_id = r.service_id
     AND we.program_start_at = r.program_start_at
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       count(*) AS recording_count,
       count(DISTINCT (l.network_id, l.service_id, l.program_start_at))
           FILTER (WHERE l.unwatched)::bigint AS unwatched_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`

const productionWithoutUnwatchedQuery = `
WITH playable_assets AS (
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
       0::bigint AS unwatched_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`

const shelfMaterializedAssetsQuery = `
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
       0::bigint AS unwatched_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`

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
watched_events AS (
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recording_watched w
    JOIN recordings r ON r.id = w.recording_id
),
live AS MATERIALIZED (
    SELECT r.id,
           r.title,
           r.program_start_at,
           r.network_id,
           r.service_id,
           rs.value,
           pa.recording_id AS playable_recording_id,
           we.network_id IS NULL AS unwatched
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    LEFT JOIN watched_events we
      ON we.network_id = r.network_id
     AND we.service_id = r.service_id
     AND we.program_start_at = r.program_start_at
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       count(*) AS recording_count,
       count(DISTINCT (l.network_id, l.service_id, l.program_start_at))
           FILTER (WHERE l.unwatched)::bigint AS unwatched_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`
