package pgstore

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/rulequery"
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
