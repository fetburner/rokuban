package rulequery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// allConditionShapes は Compile が生成しうる節をすべて 1 度は含む条件の集合。
// 新しい条件次元を足したらここにも足す。
func allConditionShapes() map[string]Conditions {
	free := true
	minMs := int64(60_000)
	maxMs := int64(7_200_000)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)

	return map[string]Conditions{
		"empty":              {},
		"sites":              {Sites: []string{"default", "other"}},
		"isFree":             {IsFree: &free},
		"duration":           {DurationMinMs: &minMs, DurationMaxMs: &maxMs},
		"period":             {PeriodStartAt: &start, PeriodEndAt: &end},
		"genres":             {Genres: []int16{0, 7, 15}},
		"services":           {Services: []ServiceRef{{NetworkID: 32736, ServiceID: 1024}, {NetworkID: 4, ServiceID: 101}}},
		"channelTypes":       {ChannelTypes: []string{"GR", "BS"}},
		"keyword":            {TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}}},
		"keywordCS":          {TextMatches: []TextMatch{{Target: "description", Mode: "keyword", Value: "News", CaseSensitive: true}}},
		"keywordNegate":      {TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "再放送", Negate: true}}},
		"regex":              {TextMatches: []TextMatch{{Target: "name", Mode: "regex", Value: "^第[0-9]+話"}}},
		"regexCS":            {TextMatches: []TextMatch{{Target: "extended", Mode: "regex", Value: "NHK", CaseSensitive: true}}},
		"times":              {Times: []TimeWindow{{Weekdays: 0b0011111, StartSec: 6 * 3600, EndSec: 9 * 3600}}},
		"timesOvernight":     {Times: []TimeWindow{{Weekdays: 0b1111111, StartSec: 23 * 3600, EndSec: 1 * 3600}}},
		"timesMultiple":      {Times: []TimeWindow{{Weekdays: 1, StartSec: 0, EndSec: 3600}, {Weekdays: 64, StartSec: 3600, EndSec: 86400}}},
		"timesFullDayBounds": {Times: []TimeWindow{{Weekdays: 127, StartSec: 0, EndSec: 86400}}},
		"everything": {
			IsFree:        &free,
			DurationMinMs: &minMs,
			DurationMaxMs: &maxMs,
			PeriodStartAt: &start,
			PeriodEndAt:   &end,
			Genres:        []int16{7},
			Services:      []ServiceRef{{NetworkID: 32736, ServiceID: 1024}},
			ChannelTypes:  []string{"GR"},
			Sites:         []string{"default"},
			TextMatches: []TextMatch{
				{Target: "name", Mode: "keyword", Value: "アニメ"},
				{Target: "description", Mode: "regex", Value: "[0-9]+", Negate: true},
			},
			Times: []TimeWindow{{Weekdays: 0b1000000, StartSec: 20 * 3600, EndSec: 22 * 3600}},
		},
	}
}

// TestCompile_AllShapesAcceptedByPostgres は生成 SQL が Postgres に受理されることを
// 全条件形について確かめる。文字列検査だけのテストでは構文エラーを検出できず、
// 実際に AT TIME ZONE の連結ミスがこの穴をすり抜けた。
func TestCompile_AllShapesAcceptedByPostgres(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	for name, c := range allConditionShapes() {
		t.Run(name, func(t *testing.T) {
			out, err := Compile("default", c)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			sql := "SELECT p.program_id FROM epg_programs p"
			if out.NeedsServiceJoin {
				sql += " JOIN epg_services s ON s.site = p.site" +
					" AND s.network_id = p.network_id AND s.service_id = p.service_id"
			}
			sql += " WHERE " + out.Where

			// 実行して初めて構文・型の誤りが出る。行が返るかは問わない。
			rows, err := pool.Query(ctx, sql, out.Args...)
			if err != nil {
				t.Fatalf("postgres rejected generated SQL: %v\nSQL: %s", err, sql)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
		})
	}
}

// TestCompile_NoUserBytesInSQL はユーザー由来の値が SQL 文字列に一切現れないことを
// 確かめる（値はすべて $N プレースホルダ経由）。列名・演算子は whitelist なので、
// この 2 つが守られている限り注入経路は存在しない。
func TestCompile_NoUserBytesInSQL(t *testing.T) {
	const payload = `'; DROP TABLE epg_programs; --`

	out, err := Compile(payload, Conditions{
		Sites:        []string{payload},
		ChannelTypes: []string{payload},
		TextMatches: []TextMatch{
			{Target: "name", Mode: "keyword", Value: payload},
			{Target: "name", Mode: "keyword", Value: payload, CaseSensitive: true},
			{Target: "description", Mode: "regex", Value: payload},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, needle := range []string{"DROP TABLE", "--", ";"} {
		if strings.Contains(out.Where, needle) {
			t.Errorf("generated SQL contains user-controlled %q: %s", needle, out.Where)
		}
	}

	// 値の数だけプレースホルダが割り当てられていること
	// （site 1 + Sites 2 + ChannelTypes 1 + TextMatches 3 = 7）
	if len(out.Args) != 7 {
		t.Errorf("len(Args) = %d, want 7 (all values parameterized)", len(out.Args))
	}
	for i := range out.Args {
		if !strings.Contains(out.Where, "$"+itoa(i+1)) {
			t.Errorf("placeholder $%d not referenced: %s", i+1, out.Where)
		}
	}
}

// TestCompile_RejectsUnknownIdentifiers は列名・演算子が whitelist であることを確かめる。
// ここが緩むと注入経路が開くので、default が error であることをテストで固定する。
func TestCompile_RejectsUnknownIdentifiers(t *testing.T) {
	cases := []TextMatch{
		{Target: "name; DROP TABLE epg_programs", Mode: "keyword", Value: "x"},
		{Target: "name", Mode: "keyword; DROP TABLE epg_programs", Value: "x"},
		{Target: "", Mode: "keyword", Value: "x"},
		{Target: "name", Mode: "", Value: "x"},
	}
	for _, m := range cases {
		if _, err := Compile("default", Conditions{TextMatches: []TextMatch{m}}); err == nil {
			t.Errorf("Compile accepted unknown identifier: target=%q mode=%q", m.Target, m.Mode)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
