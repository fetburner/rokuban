package pgstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCompile_KeywordAndGenre(t *testing.T) {
	c := rulequery.Conditions{
		Sites: []string{"default"},
		TextMatches: []rulequery.TextMatch{
			{Target: "name", Mode: "keyword", Value: "ニュース"},
		},
		Genres: []int16{0, 1},
	}
	out, err := compileRuleQuery(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.where, "p.site = ANY($1)") {
		t.Errorf("where = %s", out.where)
	}
	if !strings.Contains(out.where, "normalize_search_text") {
		t.Errorf("expected normalize_search_text for keyword: %s", out.where)
	}
	if !strings.Contains(out.where, "genre_lv1") {
		t.Errorf("expected genre: %s", out.where)
	}
	if out.needsServiceJoin {
		t.Error("should not need service join")
	}
	if len(out.args) < 2 {
		t.Fatalf("args = %#v", out.args)
	}
}

func TestCompile_ChannelTypeNeedsJoin(t *testing.T) {
	out, err := compileRuleQuery(rulequery.Conditions{Sites: []string{"default"}, ChannelTypes: []string{"BS"}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.needsServiceJoin {
		t.Error("expected service join")
	}
	if !strings.Contains(out.where, "s.channel_type") {
		t.Errorf("where = %s", out.where)
	}
}

func TestCompile_NegateRegex(t *testing.T) {
	out, err := compileRuleQuery(rulequery.Conditions{
		Sites: []string{"default"},
		TextMatches: []rulequery.TextMatch{
			{Target: "name", Mode: "regex", Value: "再放送", Negate: true, CaseSensitive: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.where, "NOT (") || !strings.Contains(out.where, " ~ ") {
		t.Errorf("where = %s", out.where)
	}
}

func TestCompile_TimeWindowOvernight(t *testing.T) {
	out, err := compileRuleQuery(rulequery.Conditions{
		Sites: []string{"default"},
		Times: []rulequery.TimeWindow{{Weekdays: 0b1111111, StartSec: 23 * 3600, EndSec: 1 * 3600}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.where, "OR") {
		t.Errorf("expected overnight OR: %s", out.where)
	}
}

func TestCompile_Period(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	out, err := compileRuleQuery(rulequery.Conditions{Sites: []string{"home"}, PeriodStartAt: &start, PeriodEndAt: &end})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.where, "p.start_at >=") || !strings.Contains(out.where, "p.start_at <") {
		t.Errorf("where = %s", out.where)
	}
}

// TestCompile_SitesFilter は Sites が実際に epg_programs.site で行を絞ることを、
// 返ってきた行そのもので主張する（#530 の罠: 旧テストは WHERE 文字列に "ANY" が
// 含まれるかしか見ておらず、列を参照しない定数述語でも同じように通っていた）。
//
// 2 site にまたがって同一 programId の行を用意し、Sites に 1 site だけを指定すると
// その site の行だけが返ることを見る。p.site 述語を落とす壊し方（例えば
// "TRUE" に縮退させる）をすると、指定していない site の行も混ざって
// 返ってくるためこのテストが落ちる。
func TestCompile_SitesFilter(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Date(2026, 8, 10, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	const programID int64 = 8001
	for _, site := range []string{"default", "cabin"} {
		insertProgramFixture(t, pool, ctx, site, programID, start)
	}

	matches, err := NewEPG(pool).SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Site != "default" || matches[0].ProgramID != programID {
		t.Fatalf("matches = %+v, want exactly [{default %d}] (cabin の行が混ざってはいけない)", matches, programID)
	}

	// 逆方向も見る（両方向で確認する。テスト規律）: Sites に両方を指定すれば両方返る。
	both, err := NewEPG(pool).SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default", "cabin"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Fatalf("both = %+v, want 2 rows (default + cabin)", both)
	}
}

func TestCompile_EmptySites(t *testing.T) {
	_, err := compileRuleQuery(rulequery.Conditions{})
	if err == nil {
		t.Fatal("expected error")
	}
}
