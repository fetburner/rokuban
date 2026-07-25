package rulequery

import (
	"strings"
	"testing"
	"time"
)

func TestCompile_KeywordAndGenre(t *testing.T) {
	c := Conditions{
		TextMatches: []TextMatch{
			{Target: "name", Mode: "keyword", Value: "ニュース"},
		},
		Genres: []int16{0, 1},
	}
	out, err := Compile("default", c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Where, "p.site = $1") {
		t.Errorf("where = %s", out.Where)
	}
	if !strings.Contains(out.Where, "normalize_search_text") {
		t.Errorf("expected normalize_search_text for keyword: %s", out.Where)
	}
	if !strings.Contains(out.Where, "genre_lv1") {
		t.Errorf("expected genre: %s", out.Where)
	}
	if out.NeedsServiceJoin {
		t.Error("should not need service join")
	}
	if len(out.Args) < 2 {
		t.Fatalf("args = %#v", out.Args)
	}
}

func TestCompile_ChannelTypeNeedsJoin(t *testing.T) {
	out, err := Compile("default", Conditions{ChannelTypes: []string{"BS"}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsServiceJoin {
		t.Error("expected service join")
	}
	if !strings.Contains(out.Where, "s.channel_type") {
		t.Errorf("where = %s", out.Where)
	}
}

func TestCompile_NegateRegex(t *testing.T) {
	out, err := Compile("default", Conditions{
		TextMatches: []TextMatch{
			{Target: "name", Mode: "regex", Value: "再放送", Negate: true, CaseSensitive: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Where, "NOT (") || !strings.Contains(out.Where, " ~ ") {
		t.Errorf("where = %s", out.Where)
	}
}

func TestCompile_TimeWindowOvernight(t *testing.T) {
	out, err := Compile("default", Conditions{
		Times: []TimeWindow{{Weekdays: 0b1111111, StartSec: 23 * 3600, EndSec: 1 * 3600}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Where, "OR") {
		t.Errorf("expected overnight OR: %s", out.Where)
	}
}

func TestCompile_Period(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	out, err := Compile("home", Conditions{PeriodStartAt: &start, PeriodEndAt: &end})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Where, "p.start_at >=") || !strings.Contains(out.Where, "p.start_at <") {
		t.Errorf("where = %s", out.Where)
	}
}

func TestCompile_SitesFilter(t *testing.T) {
	out, err := Compile("default", Conditions{Sites: []string{"default", "cabin"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Where, "ANY") {
		t.Errorf("where = %s", out.Where)
	}
}

func TestCompile_EmptySite(t *testing.T) {
	_, err := Compile("", Conditions{})
	if err == nil {
		t.Fatal("expected error")
	}
}
