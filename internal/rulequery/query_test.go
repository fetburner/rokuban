package rulequery

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ($1, $2::bigint, 32736, 1024, 1, $3::timestamptz, 1800000, $4::timestamptz, true, 'テスト番組', '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`, site, programID, start, start.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// idsOf は ProgramMatch のスライスから programId だけを取り出す
// （site を無視して集合比較したいテスト向けの小さなヘルパー）。
func idsOf(matches []ProgramMatch) []int64 {
	ids := make([]int64, len(matches))
	for i, m := range matches {
		ids[i] = m.ProgramID
	}
	return ids
}

func TestMatchProgramIDs_Integration(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	_ = sqlcgen.New(pool)

	_, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ('default', 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	inserts := []struct {
		id   int64
		off  time.Duration
		free bool
		name string
		g    string
	}{
		{1001, 0, true, "ニュース7", "{0}"},
		{1002, time.Hour, true, "アニメスペシャル", "{7}"},
		{1003, 2 * time.Hour, false, "映画", "{6}"},
	}
	for _, row := range inserts {
		st := start.Add(row.off)
		end := st.Add(30 * time.Minute)
		_, err = pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ('default', $1::bigint, 32736, 1024, $2::integer, $3::timestamptz, 1800000, $4::timestamptz, $5::boolean, $6::text, '', $7::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`,
			row.id, int32(row.id), st, end, row.free, row.name, row.g)
		if err != nil {
			t.Fatal(err)
		}
	}

	// keyword ニュース
	matches, err := MatchPrograms(ctx, pool, Conditions{
		Sites:       []string{"default"},
		TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(matches)
	if len(ids) != 1 || ids[0] != 1001 {
		t.Fatalf("keyword match = %v, want [1001]", ids)
	}

	// genre 7
	matches, err = MatchPrograms(ctx, pool, Conditions{Sites: []string{"default"}, Genres: []int16{7}})
	if err != nil {
		t.Fatal(err)
	}
	ids = idsOf(matches)
	if len(ids) != 1 || ids[0] != 1002 {
		t.Fatalf("genre match = %v, want [1002]", ids)
	}

	// is_free false
	f := false
	matches, err = MatchPrograms(ctx, pool, Conditions{Sites: []string{"default"}, IsFree: &f})
	if err != nil {
		t.Fatal(err)
	}
	ids = idsOf(matches)
	if len(ids) != 1 || ids[0] != 1003 {
		t.Fatalf("is_free match = %v, want [1003]", ids)
	}

	// channel type GR via join
	matches, err = MatchPrograms(ctx, pool, Conditions{Sites: []string{"default"}, ChannelTypes: []string{"GR"}})
	if err != nil {
		t.Fatal(err)
	}
	ids = idsOf(matches)
	if len(ids) != 3 {
		t.Fatalf("channel type GR = %v, want 3 programs", ids)
	}

	// BS → none
	matches, err = MatchPrograms(ctx, pool, Conditions{Sites: []string{"default"}, ChannelTypes: []string{"BS"}})
	if err != nil {
		t.Fatal(err)
	}
	ids = idsOf(matches)
	if len(ids) != 0 {
		t.Fatalf("channel type BS = %v, want empty", ids)
	}

	// same conditions twice → identical sets (検索と ruler の一致の核)
	c := Conditions{
		Sites:       []string{"default"},
		TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "アニメ"}},
		Genres:      []int16{7},
	}
	a, err := MatchPrograms(ctx, pool, c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MatchPrograms(ctx, pool, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || (len(a) == 1 && a[0] != b[0]) {
		t.Fatalf("idempotent match diverged: %v vs %v", a, b)
	}

	// 全角キーワードが半角番組名にマッチする（normalize_search_text）
	_, err = pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ('default', 1004, 32736, 1024, 4, $1::timestamptz, 1800000, $2::timestamptz, true, 'NHKニュース', '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`, start.Add(3*time.Hour), start.Add(3*time.Hour+30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	matches, err = MatchPrograms(ctx, pool, Conditions{
		Sites:       []string{"default"},
		TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "ＮＨＫ"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = idsOf(matches)
	if len(ids) != 1 || ids[0] != 1004 {
		t.Fatalf("fullwidth keyword match = %v, want [1004]", ids)
	}
}

// TestMatchProgramIDsForRule_RuleSitesGatesEvaluationSite は rule_sites が非空のとき、
// 対象外の site を評価すると（Compile まで進まず）クエリを投げずに空を返すことを
// 確認する。ruler は site ごとのループで MatchProgramIDsForRule を呼ぶため
// （docs/recording/ruler.md「サイトの扱い」）、対象内の site では通常どおりマッチする
// ことも合わせて見る（両方向。テスト規律）。
func TestMatchProgramIDsForRule_RuleSitesGatesEvaluationSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	const programID int64 = 9001
	for _, site := range []string{"tokyo", "osaka"} {
		insertProgramFixture(t, pool, ctx, site, programID, start)
	}

	var ruleID int64
	if err := pool.QueryRow(ctx, `INSERT INTO rules (name) VALUES ('rule_sites test') RETURNING id`).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rule_sites (rule_id, site) VALUES ($1, 'tokyo')`, ruleID); err != nil {
		t.Fatal(err)
	}

	// rule_sites = {tokyo} なので osaka は対象外 --- 空を返す。
	ids, err := MatchProgramIDsForRule(ctx, pool, "osaka", ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("osaka (対象外) ids = %v, want empty", ids)
	}

	// tokyo は対象内 --- 通常どおりマッチする。
	ids, err = MatchProgramIDsForRule(ctx, pool, "tokyo", ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != programID {
		t.Fatalf("tokyo (対象内) ids = %v, want [%d]", ids, programID)
	}
}
