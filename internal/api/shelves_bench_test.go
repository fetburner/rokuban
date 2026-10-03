package api

import (
	"context"
	"fmt"
	"os"
	"slices"
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
//   - 1,000 件（1.4%）: ごみ箱。975 件は active original あり、25 件は purged で media_asset なし
//   - 1,000 件（1.4%）: superseded、active original あり
//
// タイトルは 141 個の自動キーへ均等に分け、分類ルールを 50 本置く。ルールの値は
// 自動キーと同じなので、recording_series の JOIN と評価結果を含むプランを測れる。
// 未視聴の数え方を壊したら落ちるよう、放送イベントの重なりを seed に置く（詳細は seed の SQL コメント）。
// 別 site の重複、event_id だけが違う重複、開始時刻だけが同じ別サービス、ごみ箱にしか行が無いイベント、
// 削除済み / supersede 済み / purged の行に付いた watched 印である。
//
// 測る形は棚クエリ 6 つと予約一覧 2 つである。
//
//   - (a) 本番: sqlc の ListRecordingShelves。生きている録画を母集団にして playable_assets を
//     LEFT JOIN し、再生可能数・未視聴イベント数・latestStartAt を同じ集計から返す形
//   - (o) 旧形: 再生できる録画だけを INNER JOIN した母集団（playable を MATERIALIZED）。
//     母集団が (a) と違うので比較用のリテラルとして持つ
//   - (o') (o) から playable の MATERIALIZED を外した形。同じ母集団どうしの比較
//   - (b') (a) の live を MATERIALIZED にした形
//   - (c) 未視聴イベント数を含む比較形。playable_assets を MATERIALIZED にする
//   - (o_inline) (o) の recording_series を書き下した形。予算の 141 ms を測った形で、比の分母
//   - (d) / (d') 予約一覧に実効シリーズを LEFT JOIN / 相関サブクエリで足す形。予約数と、予約に
//     載らない EPG の行数を別々に動かす
//
// 各形を同じ接続で、形を交互に回すラウンド 10 回ずつ実行し（実行順の偏りを消す）、
// 中央値を t.Logf に出す。比も出すが、判定には使わない。
//
// 判定しているのは結果の一致だけである。棚ごとに (a) の playable_count と (o) の recording_count
// （母集団が違うのでこの対応で見る）、(o') / (o_inline) と (o) の全列、(b') と (a) の全列が一致し、
// (a) の latest_start_at は別クエリで求めた「その棚の生きている録画の program_start_at の最大値」と
// 一致する。(a) の本番 SQL の max や FILTER を壊すとここで落ちる。
// (c) は (a) の全列に加えて、独立に集計した「再生可能な live 放送イベントのうち、
// 全録画行を通じて watched 印が無いイベント数」と一致し、合計は 64,600 である。
// (d') は (d) と予約ごとの実効シリーズが一致する。
//
// 既知の 617 ms（playable の MATERIALIZED を外すと数倍遅い）は、現スキーマ・この合成
// seed では再現しない。617 ms の再現条件は未検証なので (o') が遅いことはアサートしない。
//
// 渡された DB はマイグレーション済みであることを前提とし、seed の冒頭で棚用の
// recordings / media_assets / recording_watched / label_rules / label_rule_hits を無条件に TRUNCATE する。
// 棚の測定の後、予約用の reservations / program_snapshots / epg_programs も TRUNCATE する。
// CASCADE で program_intents / program_overrides も消える（必ず専用 DB を渡す）。
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
					unwatched: r.UnwatchedCount,
					latest:    r.LatestStartAt, representative: r.RepresentativeID,
				}
			}
			return out, nil
		}},
		{name: "(o) previous playable-only shape", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), previousShelfQuery)
		}},
		{name: "(o') previous shape without playable MATERIALIZED", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), previousUnmaterializedShelfQuery)
		}},
		{name: "(b') production shape with live MATERIALIZED", run: func() (map[string]shelfResult, error) {
			return queryFullShelves(ctx, conn.Conn(), liveMaterializedShelfQuery)
		}},
		{name: "(c) production shelf with unwatched broadcast-event counts", run: func() (map[string]shelfResult, error) {
			return queryFullShelves(ctx, conn.Conn(), unwatchedShelfQuery)
		}},
		{name: "(o_inline) previous shape with the effective series written inline", run: func() (map[string]shelfResult, error) {
			return queryShelves(ctx, conn.Conn(), previousInlineShelfQuery)
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

	production, previous, previousUnmaterialized, liveMaterialized, withUnwatched, previousInline := results[0], results[1], results[2], results[3], results[4], results[5]
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
		if previousInline[key] != prev {
			t.Errorf("shelf %q: (o_inline) %+v != (o) %+v", key, previousInline[key], prev)
		}
		if prod.playable != prev.recording {
			t.Errorf("shelf %q: (a) playable_count %d != (o) recording_count %d", key, prod.playable, prev.recording)
		}
		if want, ok := wantLatest[key]; !ok || !prod.latest.Equal(want) {
			t.Errorf("shelf %q: (a) latest_start_at %v != expected %v", key, prod.latest, want)
		}
		if want := wantUnwatched[key]; prod.unwatched != want {
			t.Errorf("shelf %q: (a) unwatched count %d != expected %d", key, prod.unwatched, want)
		}
		if got, ok := liveMaterialized[key]; !ok || got != prod {
			t.Errorf("shelf %q: (b') %+v != (a) %+v", key, liveMaterialized[key], prod)
		}
		gotUnwatched, ok := withUnwatched[key]
		if !ok {
			t.Errorf("(c) missing shelf %q", key)
			continue
		}
		if gotUnwatched.title != prod.title || gotUnwatched.recording != prod.recording ||
			gotUnwatched.playable != prod.playable || !gotUnwatched.latest.Equal(prod.latest) ||
			gotUnwatched.unwatched != prod.unwatched || gotUnwatched.representative != prod.representative {
			t.Errorf("(c) shelf %q base fields %+v != (a) %+v", key, gotUnwatched, prod)
		}
		if gotUnwatched.unwatched < 0 || gotUnwatched.unwatched > gotUnwatched.playable {
			t.Errorf("(c) shelf %q unwatched count %d is outside [0, playable %d]", key, gotUnwatched.unwatched, gotUnwatched.playable)
		}
		if want := wantUnwatched[key]; gotUnwatched.unwatched != want {
			t.Errorf("(c) shelf %q unwatched count = %d, want %d", key, gotUnwatched.unwatched, want)
		}
		unwatched += prod.unwatched
		playable += prod.playable
		live += prod.recording
	}
	if playable != 65_000 || live != 71_000 {
		t.Errorf("production totals = playable %d / live %d, want 65000 / 71000", playable, live)
	}
	if unwatched != 64_600 {
		t.Errorf("(c) total unwatched events = %d, want 64600", unwatched)
	}

	// 予算 200 ms は、(o_inline) の形が 141 ms だった環境で決めた。そのため同じ回の (o_inline) との比
	// 200/141 で読む。当時は 73,000 行すべてが再生可能だったが、この seed では 65,000 行である。
	// この違いが比をどちらへ動かすかは未検証。
	t.Logf("ratios to (a): (o)=%.2f, (o')=%.2f, (b')=%.2f; (o')/(o)=%.2f; (a)/(o_inline)=%.2f; (o)/(o_inline)=%.2f; (c)/(o_inline)=%.2f (budget 200/141=%.2f)",
		float64(medians[1])/float64(medians[0]), float64(medians[2])/float64(medians[0]),
		float64(medians[3])/float64(medians[0]), float64(medians[2])/float64(medians[1]),
		float64(medians[0])/float64(medians[5]), float64(medians[1])/float64(medians[5]),
		float64(medians[4])/float64(medians[5]), 200.0/141.0)

	benchmarkReservationsWithSeries(t, ctx, conn.Conn())
}

// benchmarkReservationsWithSeries は予約一覧に実効シリーズを足す 2 形を、予約数と EPG の行数を
// 別々に動かして測る。epg_program_series は EPG の全行で label_rule_winner を評価しうるので、
// 予約と無関係な EPG 行を足した点を含める。
func benchmarkReservationsWithSeries(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	points := []struct {
		reservations, unrelatedEPG, nullSeries int
	}{
		{500, 0, 5},
		{2_000, 0, 20},
		{2_000, 134_000, 20},
	}
	seeded := 0
	for _, p := range points {
		if p.reservations > seeded {
			seedReservationBenchmarkRange(t, conn, seeded+1, p.reservations)
			seeded = p.reservations
		}
		if p.unrelatedEPG > 0 {
			seedUnrelatedEPGPrograms(t, conn, p.unrelatedEPG)
		}
		got, err := measureReservationsWithSeries(ctx, conn)
		if err != nil {
			t.Fatalf("measuring reservations at %d reservations / +%d EPG rows: %v", p.reservations, p.unrelatedEPG, err)
		}
		for i, shape := range got {
			if shape.rows != p.reservations || shape.nullSeries != p.nullSeries {
				t.Errorf("%s at %d reservations / +%d EPG rows returned %d rows with %d null series, want %d / %d",
					reservationSeriesShapes[i].name, p.reservations, p.unrelatedEPG, shape.rows, shape.nullSeries, p.reservations, p.nullSeries)
			}
			if !slices.Equal(shape.series, got[0].series) {
				t.Errorf("%s series differ from %s at %d reservations / +%d EPG rows",
					reservationSeriesShapes[i].name, reservationSeriesShapes[0].name, p.reservations, p.unrelatedEPG)
			}
			t.Logf("%s, %d reservations, +%d unrelated EPG rows: median %s (null series %d)",
				reservationSeriesShapes[i].name, p.reservations, p.unrelatedEPG, shape.median, shape.nullSeries)
		}
	}
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

// queryShelves は旧形の棚クエリを実行して棚ごとの結果を返す。
func queryShelves(ctx context.Context, conn *pgx.Conn, query string) (map[string]shelfResult, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]shelfResult{}
	for rows.Next() {
		var value pgtype.Text
		var r shelfResult
		if err := rows.Scan(&value, &r.title, &r.recording, &r.representative); err != nil {
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

// queryFullShelves は本番と同じ列順の 7 列を返す棚クエリを実行する。
func queryFullShelves(ctx context.Context, conn *pgx.Conn, query string) (map[string]shelfResult, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]shelfResult{}
	for rows.Next() {
		var value pgtype.Text
		var r shelfResult
		if err := rows.Scan(&value, &r.title, &r.recording, &r.playable, &r.unwatched, &r.latest, &r.representative); err != nil {
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

// seedUnrelatedEPGPrograms は予約に載らない EPG 行を足す。番組名は予約側と同じ 141 系列に散らす。
func seedUnrelatedEPGPrograms(t *testing.T, conn *pgx.Conn, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id, start_at, duration_ms,
  end_at, is_free, name
)
SELECT
  'bench', 1000000 + i, 32678, 5169, i,
  timestamptz '2026-01-01 00:00:00+00' + i * interval '1 minute', 1800000,
  timestamptz '2026-01-01 00:00:00+00' + i * interval '1 minute' + interval '30 minutes',
  true, format('シリーズ%s 第%s回', lpad(((i - 1) % 141)::text, 3, '0'), i)
FROM generate_series(1, $1::integer) AS s(i);
`, n); err != nil {
		t.Fatalf("seeding %d unrelated EPG programs: %v", n, err)
	}
	if _, err := conn.Exec(ctx, "ANALYZE epg_programs"); err != nil {
		t.Fatalf("analyzing epg_programs: %v", err)
	}
}

// reservationSeriesShape は 1 形ぶんの測定結果である。series は予約一覧の順に並べた実効シリーズで、
// 形どうしの一致を見る（NULL は空文字ではなく "<null>" にする）。
type reservationSeriesShape struct {
	median     time.Duration
	rows       int
	nullSeries int
	series     []string
}

// measureReservationsWithSeries は reservationSeriesShapes を交互に 10 ラウンド回し、形ごとの中央値と
// 最終ラウンドの結果を返す。ラウンド間で結果が変わったらエラーにする。
func measureReservationsWithSeries(ctx context.Context, conn *pgx.Conn) ([]reservationSeriesShape, error) {
	const rounds = 10
	out := make([]reservationSeriesShape, len(reservationSeriesShapes))
	samples := make([][]time.Duration, len(reservationSeriesShapes))
	for round := 0; round < rounds; round++ {
		for i, shape := range reservationSeriesShapes {
			started := time.Now()
			series, err := queryReservationSeries(ctx, conn, shape.query)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", shape.name, err)
			}
			samples[i] = append(samples[i], time.Since(started))
			if round > 0 && !slices.Equal(series, out[i].series) {
				return nil, fmt.Errorf("%s: result changed between rounds", shape.name)
			}
			out[i].series = series
		}
	}
	for i := range out {
		out[i].median = median(samples[i])
		out[i].rows = len(out[i].series)
		for _, v := range out[i].series {
			if v == "<null>" {
				out[i].nullSeries++
			}
		}
	}
	return out, nil
}

func queryReservationSeries(ctx context.Context, conn *pgx.Conn, query string) ([]string, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var series []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		if len(values) != 24 {
			return nil, fmt.Errorf("returned %d columns, want 24", len(values))
		}
		if values[23] == nil {
			series = append(series, "<null>")
		} else {
			series = append(series, fmt.Sprint(values[23]))
		}
	}
	return series, rows.Err()
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
           WHEN i BETWEEN 1101 AND 1200 THEN i - 900
           WHEN i BETWEEN 1201 AND 1250 THEN i - 1100
           WHEN i BETWEEN 71001 AND 71900 THEN i - 71000
           WHEN i BETWEEN 71901 AND 72000 THEN i - 70800
           WHEN i BETWEEN 72001 AND 73000 THEN i - 71950
           ELSE i
         END AS event_index,
         CASE WHEN i BETWEEN 1001 AND 1200 THEN 'secondary' ELSE 'default' END AS site,
         CASE WHEN i BETWEEN 1201 AND 1250 THEN 5169 ELSE 5168 END AS service_id,
         CASE WHEN i BETWEEN 1001 AND 1050 THEN 100000 ELSE 0 END AS event_id_offset
  FROM generate_series(1, 73000) AS s(i)
)
INSERT INTO recordings (
  source, site, network_id, service_id, event_id, service_name, channel_type, channel,
  title, program_start_at, program_duration_ms, status, deleted_at, superseded_at, purged_at
)
SELECT
  'manual', site, 32678, service_id, event_index + event_id_offset, 'benchmark', 'GR', '27',
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
  CASE WHEN i BETWEEN 72001 AND 73000 THEN now() ELSE NULL END,
  CASE WHEN i BETWEEN 71001 AND 71025 THEN now() ELSE NULL END
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
WHERE i <= 65000 OR i BETWEEN 71026 AND 73000;

INSERT INTO label_rules (key, value, keyword, priority)
SELECT
  'series',
  format('シリーズ%s', lpad(i::text, 3, '0')),
  format('シリーズ%s', lpad(i::text, 3, '0')),
  50 - i
FROM generate_series(0, 49) AS s(i);

-- Events 1..100 and 201..300 are present at two live sites. Watched marks on deleted and superseded rows
-- cover events 1..100 and live marks cover 101..200, so 201..300 stay unwatched at both sites;
-- counting rows instead of events double-counts them. The secondary rows for events 1..50 carry a
-- different event_id, so keying by event_id loses their marks. Marks on rows 71001..71025 sit on
-- purged rows without media. Service 5169 repeats the start times of events 101..150 unwatched, so
-- dropping the service from the key borrows their marks. Deleted rows 71901..72000 are the only rows
-- for events 1101..1200, so counting trash rows adds them.
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

// previousInlineShelfQuery は (o) の recording_series ビューを、ビューと同じ COALESCE に書き下した形である。
// 予算の 141 ms を測ったのはこの形なので、比の分母にだけ使う。本番は書き下さない。
const previousInlineShelfQuery = `
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
           COALESCE(lr.value_key, r.series_key) AS value
    FROM recordings r
    JOIN playable_assets pa ON pa.recording_id = r.id
    LEFT JOIN label_rule_hits h ON h.recording_id = r.id
    LEFT JOIN label_rules lr ON lr.id = h.label_rule_id
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
WITH playable_assets AS (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
watched_events AS MATERIALIZED (
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_watched w ON w.recording_id = r.id
),
live AS MATERIALIZED (
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
       count(*) AS recording_count,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       (count(DISTINCT (l.network_id, l.service_id, l.program_start_at))
           FILTER (WHERE l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL))::bigint AS unwatched_count,
       max(l.program_start_at)::timestamptz AS latest_start_at,
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
       count(*) AS recording_count,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       (count(DISTINCT (l.network_id, l.service_id, l.program_start_at))
           FILTER (WHERE l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL))::bigint AS unwatched_count,
       max(l.program_start_at)::timestamptz AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST
`

// reservationsWithSeriesTemplate は sqlc の ListReservationsFull と同じ投影・結合を保ち、
// epg_program_series の値だけを追加で読む測定用クエリである。EPG から消失しても snapshot が
// 残っている予約は一覧に残り、series は NULL になる。
const reservationsWithSeriesTemplate = `
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
       %s AS series
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
%sORDER BY r.site, s.start_at
`

// reservationSeriesShapes は実効シリーズの読み方 2 形である。JOIN 形はビューを予約で絞れず、
// EPG の全行で label_rule_winner を評価しうる。相関形は予約 1 件ごとに
// (site, program_id) でビューを引く。
var reservationSeriesShapes = []struct {
	name, query string
}{
	{"(d) ListReservationsFull LEFT JOIN epg_program_series", fmt.Sprintf(reservationsWithSeriesTemplate,
		"eps.value", "LEFT JOIN epg_program_series eps ON eps.site = r.site AND eps.program_id = r.program_id\n")},
	{"(d') ListReservationsFull with correlated epg_program_series", fmt.Sprintf(reservationsWithSeriesTemplate,
		"series.value", "LEFT JOIN LATERAL (SELECT (SELECT eps.value FROM epg_program_series eps WHERE eps.site = r.site AND eps.program_id = r.program_id) AS value) series ON true\n")},
}
