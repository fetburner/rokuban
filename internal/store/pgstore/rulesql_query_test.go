package pgstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// insertProgramFixture は site の epg_services + epg_programs に programID の行を
// 1 件挿入する（rulequery パッケージのテスト共有ヘルパー）。テストごとに違う
// network_id/service_id を使う理由がないので固定値にする。
func insertProgramFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, programID int64, start time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, site); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description
) VALUES ($1, $2::bigint, 32736, 1024, 1, $3::timestamptz, 1800000, $4::timestamptz, true, 'テスト番組', '')
ON CONFLICT (site, program_id) DO NOTHING`, site, programID, start, start.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// sqlCapturingTracer は pool へ実際に流れた SQL 文字列を素朴に記録する
// pgx.QueryTracer。ホットパスが狭い射影を使っていることを、Go の定数比較では
// なく実行された SQL そのものから確認するために使う。
type sqlCapturingTracer struct {
	sqls []string
}

func (tr *sqlCapturingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.sqls = append(tr.sqls, data.SQL)
	return ctx
}

func (*sqlCapturingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestMatchProgramIDsForRule_NarrowProjection は ruler が呼ぶホットパスが、
// MatchPrograms の表示用 6 列（name は trgm 索引付きの text）を選ばず、
// programId だけの狭い射影で epg_programs に問い合わせることを確認する。
// 実装の定数と比較するのではなく、実際に pool へ流れた SQL 文字列を見る。
//
// **SELECT リストは `NeedsServiceJoin` で分岐した 2 本ある**（epg_services を
// JOIN する側としない側）ので両方を通す --- 片方だけ見ると、もう片方を広い射影に
// 戻しても緑のまま通る（実測: join 側だけを広げる変異でこのテストは通り続けた）。
func TestMatchProgramIDsForRule_NarrowProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	const programID int64 = 9101
	insertProgramFixture(t, pool, ctx, "default", programID, start)

	tracer := &sqlCapturingTracer{}
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("creating traced pool: %v", err)
	}
	defer traced.Close()

	for _, tt := range []struct {
		name string
		// channelType が空でない条件は epg_services への JOIN を要求する
		// （compile.go の `s.channel_type = ANY(...)`）。フィクスチャの局は 'GR'
		// なので、JOIN する側でも同じ 1 件がマッチする。
		channelType string
		wantJoin    bool
	}{
		{name: "join なし", channelType: "", wantJoin: false},
		{name: "epg_services を JOIN する条件", channelType: "GR", wantJoin: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ruleID int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO rules (name) VALUES ($1) RETURNING id`, tt.name).Scan(&ruleID); err != nil {
				t.Fatal(err)
			}
			if tt.channelType != "" {
				if _, err := pool.Exec(ctx,
					`INSERT INTO rule_channel_types (rule_id, channel_type) VALUES ($1, $2)`,
					ruleID, tt.channelType); err != nil {
					t.Fatal(err)
				}
			}

			tracer.sqls = nil
			ids, err := NewEPG(traced).MatchProgramIDsForRule(ctx, "default", ruleID)
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != 1 || ids[0] != programID {
				t.Fatalf("ids = %v, want [%d]", ids, programID)
			}

			var epgQuery string
			for _, sql := range tracer.sqls {
				if strings.Contains(sql, "FROM epg_programs") {
					epgQuery = sql
				}
			}
			if epgQuery == "" {
				t.Fatal("epg_programs へのクエリが記録されていない")
			}
			// 意図した分岐を実際に通っているか（通っていなければ、この
			// サブテストは狙った SELECT リストを一度も見ていない）。
			if got := strings.Contains(epgQuery, "JOIN epg_services"); got != tt.wantJoin {
				t.Fatalf("JOIN epg_services = %t, want %t: %s", got, tt.wantJoin, epgQuery)
			}
			if strings.Contains(epgQuery, "p.name") {
				t.Fatalf("MatchProgramIDsForRule が表示用の広い射影（p.name 含む）を選んでいる: %s", epgQuery)
			}
			if !strings.Contains(epgQuery, "p.program_id") {
				t.Fatalf("MatchProgramIDsForRule が program_id を選んでいない: %s", epgQuery)
			}
		})
	}
}
