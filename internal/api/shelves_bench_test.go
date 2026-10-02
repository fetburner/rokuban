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
// タイトルは 141 個の自動キーへ均等に分け、分類ルールを 50 本置く。ルールの値は
// 自動キーと同じなので、recording_series の JOIN と評価結果を含むプランを測れる。
// 100 件は別 site に同じ放送イベントの live 録画を置く。削除済み / supersede 済み行にも
// watched 印を置き、読み取り時に全行を束ねて同じイベントを 1 回だけ数える条件を検査する。
//
// 測る形は棚クエリ 5 つと予約一覧 2 点である。
//
//   - (a) 本番: sqlc の ListRecordingShelves。生きている録画を母集団にして playable_assets を
//     LEFT JOIN し、見られる件数（count FILTER）と latestStartAt（max）を同じ集計から返す形
//   - (o) 旧形: 再生できる録画だけを INNER JOIN した母集団（playable を MATERIALIZED）。
//     母集団が (a) と違うので比較用のリテラルとして持つ
//   - (o') (o) から playable の MATERIALIZED を外した形。同じ母集団どうしの比較
//   - (b') (a) の live を MATERIALIZED にした形
//   - (c) (a) に放送イベント単位の未視聴件数を加えた形
//
// 各形を同じ接続で、形を交互に回すラウンド 10 回ずつ実行し（実行順の偏りを消す）、
// 中央値を t.Logf に出す。(a) との比も出すが、判定には使わない。
//
// 判定しているのは結果の一致だけである。棚ごとに (a) の playable_count と (o) の recording_count
// （母集団が違うのでこの対応で見る）、(o') と (o) の全列、(b') と (a) の全列が一致し、
// (a) の latest_start_at は別クエリで求めた「その棚の生きている録画の program_start_at の最大値」と
// 一致する。(a) の本番 SQL の max や FILTER を壊すとここで落ちる。
// (c) は (a) の全列に加えて、独立に集計した「再生可能な live 放送イベントのうち、
// 全録画行を通じて watched 印が無いイベント数」と一致する。
//
// 既知の 617 ms（playable の MATERIALIZED を外すと数倍遅い）は、現スキーマ・この合成
// seed では再現しない。617 ms の再現条件は未検証なので (o') が遅いことはアサートしない。
//
// 渡された DB はマイグレーション済みであることを前提とし、seed の冒頭で棚用の
// recordings / media_assets / recording_watched / label_rules / label_rule_hits と、予約用の
// reservations / program_snapshots / epg_programs を無条件に TRUNCATE する（必ず専用 DB を渡す）。
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
		{name: "(c) production shelf with unwatched broadcast-event counts", run: func() (map[string]shelfResult, error) {
			return queryUnwatchedShelves(ctx, conn.Conn())
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

	production, previous, previousUnmaterialized, liveMaterialized, withUnwatched := results[0], results[1], results[2], results[3], results[4]
	wantLatest, err := queryExpectedLatest(ctx, conn.Conn())
	if err != nil {
		t.Fatalf("computing expected latest_start_at: %v", err)
	}
	wantUnwatched, err := queryExpectedUnwatched(ctx, conn.Conn())
	if err != nil {
		t.Fatalf("computing expected unwatched event counts: %v", err)
	}
	var playable, live, unwatched int64
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
		gotUnwatched, ok := withUnwatched[key]
		if !ok {
			t.Errorf("(c) missing shelf %q", key)
			continue
		}
		if gotUnwatched.title != prod.title || gotUnwatched.recording != prod.recording ||
			gotUnwatched.playable != prod.playable || !gotUnwatched.latest.Equal(prod.latest) ||
			gotUnwatched.representative != prod.representative {
			t.Errorf("(c) shelf %q base fields %+v != (a) %+v", key, gotUnwatched, prod)
		}
		if gotUnwatched.unwatched < 0 || gotUnwatched.unwatched > gotUnwatched.playable {
			t.Errorf("(c) shelf %q unwatched count %d is outside [0, playable %d]", key, gotUnwatched.unwatched, gotUnwatched.playable)
		}
		if want := wantUnwatched[key]; gotUnwatched.unwatched != want {
			t.Errorf("(c) shelf %q unwatched count = %d, want %d", key, gotUnwatched.unwatched, want)
		}
		unwatched += gotUnwatched.unwatched
		playable += prod.playable
		live += prod.recording
	}
	if playable != 65_000 || live != 71_000 {
		t.Errorf("production totals = playable %d / live %d, want 65000 / 71000", playable, live)
	}
	if unwatched != 64_700 {
		t.Errorf("(c) total unwatched events = %d, want 64700", unwatched)
	}

	t.Logf("ratios to (a): (o)=%.2f, (o')=%.2f, (b')=%.2f; (o')/(o)=%.2f; budget=200ms; absolute comparison to the original 141ms environment is not established here",
		float64(medians[1])/float64(medians[0]), float64(medians[2])/float64(medians[0]),
		float64(medians[3])/float64(medians[0]), float64(medians[2])/float64(medians[1]))

	benchmarkReservationsWithSeries(t, ctx, conn.Conn())
}

func benchmarkReservationsWithSeries(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	seedReservationBenchmarkRange(t, conn, 1, 500)
	reservationMedian, reservationCount, nullSeriesCount, err := measureReservationsWithSeries(ctx, conn)
	if err != nil {
		t.Fatalf("measuring ListReservationsFull + epg_program_series at 500 reservations: %v", err)
	}
	if reservationCount != 500 || nullSeriesCount != 5 {
		t.Errorf("(d) 500 reservations returned %d rows with %d null series, want 500 / 5", reservationCount, nullSeriesCount)
	}
	t.Logf("(d) ListReservationsFull + epg_program_series, 500 reservations: median %s (null series %d)", reservationMedian, nullSeriesCount)

	seedReservationBenchmarkRange(t, conn, 501, 2_000)
	reservationMedian, reservationCount, nullSeriesCount, err = measureReservationsWithSeries(ctx, conn)
	if err != nil {
		t.Fatalf("measuring ListReservationsFull + epg_program_series at 2000 reservations: %v", err)
	}
	if reservationCount != 2_000 || nullSeriesCount != 20 {
		t.Errorf("(d) 2000 reservations returned %d rows with %d null series, want 2000 / 20", reservationCount, nullSeriesCount)
	}
	t.Logf("(d) ListReservationsFull + epg_program_series, 2000 reservations: median %s (null series %d)", reservationMedian, nullSeriesCount)
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

func queryExpectedUnwatched(ctx context.Context, conn *pgx.Conn) (map[string]int64, error) {
	rows, err := conn.Query(ctx, `
WITH playable_events AS (
    SELECT DISTINCT rs.value, r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
      AND EXISTS (
          SELECT 1 FROM media_assets ma
          WHERE ma.recording_id = r.id
            AND ((ma.kind = 'original' AND ma.state <> 'deleted')
              OR (ma.kind = 'encoded' AND ma.state = 'active'))
      )
)
SELECT e.value, count(*)
FROM playable_events e
WHERE NOT EXISTS (
    SELECT 1
    FROM recordings any_recording
    JOIN recording_watched w ON w.recording_id = any_recording.id
    WHERE any_recording.network_id = e.network_id
      AND any_recording.service_id = e.service_id
      AND any_recording.program_start_at = e.program_start_at
)
GROUP BY e.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
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

func queryUnwatchedShelves(ctx context.Context, conn *pgx.Conn) (map[string]shelfResult, error) {
	rows, err := conn.Query(ctx, unwatchedShelfQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]shelfResult{}
	for rows.Next() {
		var value pgtype.Text
		var r shelfResult
		if err := rows.Scan(&value, &r.title, &r.playable, &r.recording, &r.unwatched, &r.latest, &r.representative); err != nil {
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

func seedReservationBenchmarkRange(t *testing.T, conn *pgx.Conn, first, last int) {
	t.Helper()
	ctx := context.Background()
	if first == 1 {
		if _, err := conn.Exec(ctx, `
TRUNCATE reservations, program_snapshots, epg_programs RESTART IDENTITY CASCADE;
`, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("truncating reservation benchmark tables: %v", err)
		}
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms, network_id, service_id,
  channel_type, channel, event_id, service_name
)
SELECT
  'bench', i, format('シリーズ%s 第%s回', lpad(((i - 1) % 141)::text, 3, '0'), i),
  timestamptz '2026-01-01 00:00:00+00' + i * interval '1 minute', 1800000,
  32678, 5168, 'GR', '27', i, 'benchmark'
FROM generate_series($1::integer, $2::integer) AS s(i);
`, first, last); err != nil {
		t.Fatalf("seeding program snapshots %d..%d: %v", first, last, err)
	}

	if _, err := conn.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id, start_at, duration_ms,
  end_at, is_free, name
)
SELECT
  'bench', i, 32678, 5168, i,
  timestamptz '2026-01-01 00:00:00+00' + i * interval '1 minute', 1800000,
  timestamptz '2026-01-01 00:00:00+00' + i * interval '1 minute' + interval '30 minutes',
  true, format('シリーズ%s 第%s回', lpad(((i - 1) % 141)::text, 3, '0'), i)
FROM generate_series($1::integer, $2::integer) AS s(i)
WHERE i % 100 <> 0;
`, first, last); err != nil {
		t.Fatalf("seeding EPG programs %d..%d: %v", first, last, err)
	}

	if _, err := conn.Exec(ctx, `
INSERT INTO reservations (site, program_id)
SELECT 'bench', i FROM generate_series($1::integer, $2::integer) AS s(i);
	`, first, last); err != nil {
		t.Fatalf("seeding reservation benchmark rows %d..%d: %v", first, last, err)
	}
	if _, err := conn.Exec(ctx, "ANALYZE reservations, program_snapshots, epg_programs"); err != nil {
		t.Fatalf("analyzing reservation benchmark tables: %v", err)
	}
}

func measureReservationsWithSeries(ctx context.Context, conn *pgx.Conn) (time.Duration, int, int, error) {
	const rounds = 10
	samples := make([]time.Duration, 0, rounds)
	var rowCount, nullSeriesCount int
	for round := 0; round < rounds; round++ {
		started := time.Now()
		rows, err := conn.Query(ctx, reservationsWithSeriesQuery)
		if err != nil {
			return 0, 0, 0, err
		}
		gotCount, gotNullSeriesCount := 0, 0
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				rows.Close()
				return 0, 0, 0, err
			}
			if len(values) != 24 {
				rows.Close()
				return 0, 0, 0, fmt.Errorf("ListReservationsFull + epg_program_series returned %d columns, want 24", len(values))
			}
			if values[23] == nil {
				gotNullSeriesCount++
			}
			gotCount++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, 0, 0, err
		}
		if round == 0 {
			rowCount, nullSeriesCount = gotCount, gotNullSeriesCount
		} else if gotCount != rowCount || gotNullSeriesCount != nullSeriesCount {
			return 0, 0, 0, fmt.Errorf("reservation result changed between rounds: rows/null series %d/%d then %d/%d", rowCount, nullSeriesCount, gotCount, gotNullSeriesCount)
		}
		samples = append(samples, time.Since(started))
	}
	return median(samples), rowCount, nullSeriesCount, nil
}

func seedShelfBenchmark(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
TRUNCATE recordings, media_assets, recording_watched, label_rules, label_rule_hits RESTART IDENTITY CASCADE;

WITH seed AS (
  SELECT i,
         CASE
           WHEN i BETWEEN 1001 AND 1100 THEN i - 1000
           WHEN i BETWEEN 71001 AND 72000 THEN i - 71000
           WHEN i BETWEEN 72001 AND 73000 THEN i - 71950
           ELSE i
         END AS event_index,
         CASE WHEN i BETWEEN 1001 AND 1100 THEN 'secondary' ELSE 'default' END AS site
  FROM generate_series(1, 73000) AS s(i)
)
INSERT INTO recordings (
  source, site, network_id, service_id, event_id, service_name, channel_type, channel,
  title, program_start_at, program_duration_ms, status, deleted_at, superseded_at
)
SELECT
  'manual', site, 32678, 5168, event_index, 'benchmark', 'GR', '27',
  format('シリーズ%s 第%s回', lpad(((event_index - 1) % 141)::text, 3, '0'), event_index),
  timestamptz '2020-01-01 00:00:00+00' + event_index * interval '1 minute',
  1800000,
  CASE
    WHEN i <= 65000 THEN 'finished'
    WHEN i <= 68000 THEN 'recording'
    WHEN i <= 70000 THEN 'finished'
    ELSE 'failed'
  END,
  CASE WHEN i BETWEEN 71001 AND 72000 THEN now() ELSE NULL END,
  CASE WHEN i BETWEEN 72001 AND 73000 THEN now() ELSE NULL END
FROM seed;

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

-- 100 live events are present at two sites; watched marks on deleted and superseded rows
-- exercise the event-wide read population. Together these mark events 1..200 exactly once.
INSERT INTO recording_watched (recording_id)
SELECT id
FROM recordings
WHERE id BETWEEN 101 AND 200
   OR id BETWEEN 71001 AND 71050
   OR id BETWEEN 72001 AND 72050;
	`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("seeding shelf benchmark: %v", err)
	}

	if _, err := sqlcgen.New(conn).ApplyLabelRuleReevaluation(ctx); err != nil {
		t.Fatalf("evaluating benchmark label rules: %v", err)
	}
	// VACUUM は暗黙のトランザクションになる複数文の送信では実行できないので 1 文ずつ送る。
	for _, table := range []string{"recordings", "media_assets", "recording_watched", "label_rules", "label_rule_hits"} {
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

const unwatchedShelfQuery = `
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
watched_events AS MATERIALIZED (
    -- 印を束ねる側は status を絞らない。ごみ箱 / supersede 済みの行の印も読む。
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_watched w ON w.recording_id = r.id
),
live AS (
    SELECT r.id,
           r.title,
           r.program_start_at,
           r.network_id,
           r.service_id,
           rs.value,
           pa.recording_id AS playable_recording_id,
           we.network_id AS watched_network_id
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
           FILTER (WHERE l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL) AS unwatched_count,
       max(l.program_start_at) AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`

// reservationsWithSeriesQuery は sqlc の ListReservationsFull と同じ投影・結合を保ち、
// epg_program_series の値だけを追加で読む測定用クエリである。LEFT JOIN により、
// EPG から消失しても snapshot が残っている予約は一覧に残り、series は NULL になる。
const reservationsWithSeriesQuery = `
SELECT r.site, r.program_id, r.rule_id, r.base, r.created_at, r.updated_at, r.dedup_match_recording_id, r.dedup_similarity,
       s.site, s.program_id, s.title, s.start_at, s.duration_ms, s.network_id, s.service_id, s.channel_type, s.channel, s.updated_at, s.event_id, s.service_name,
       i.action AS intent_action, o.overrides AS overrides,
       (EXISTS (
           SELECT 1 FROM never_scheduled_events nse
           WHERE nse.site = r.site
             AND nse.network_id = s.network_id
             AND nse.service_id = s.service_id
             AND nse.event_id = s.event_id
       ) AND NOT EXISTS (
           SELECT 1 FROM recordings rec
           WHERE rec.site = r.site
             AND rec.network_id = s.network_id
             AND rec.service_id = s.service_id
             AND rec.event_id = s.event_id
       ))::boolean AS never_recorded,
       eps.value AS series
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
LEFT JOIN epg_program_series eps ON eps.site = r.site AND eps.program_id = r.program_id
ORDER BY r.site, s.start_at
`
