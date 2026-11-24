package contracttest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/store"
)

// seedRuleQueryProgram はルール検索用の局と番組を同期経由で作る。
func seedRuleQueryProgram(t *testing.T, f EPGFixture, ctx context.Context, site string, programID int64, start time.Time, name string) {
	t.Helper()
	service := ruleQueryService(site)
	if err := f.Sync.UpsertServices(ctx, []store.EPGServiceUpsert{service}); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync.UpsertPrograms(ctx, []store.EPGProgramUpsert{ruleQueryProgram(site, programID, 1, start, true, name, nil)}); err != nil {
		t.Fatal(err)
	}
}

func ruleQueryService(site string) store.EPGServiceUpsert {
	return store.EPGServiceUpsert{
		Site: site, NetworkID: 32736, ServiceID: 1024, Type: 1,
		RemoteControlKeyID: 1, Name: "テスト局", ChannelType: "GR", Channel: "27",
	}
}

func ruleQueryProgram(site string, id int64, eventID int32, start time.Time, free bool, name string, genres json.RawMessage) store.EPGProgramUpsert {
	return store.EPGProgramUpsert{
		Site: site, ProgramID: id, NetworkID: 32736, ServiceID: 1024, EventID: eventID,
		StartAt: start, DurationMs: 1_800_000, EndAt: start.Add(30 * time.Minute),
		IsFree: free, Name: name, Genres: genres,
	}
}

func runEPGSearchIntegration(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.Sync.UpsertServices(ctx, []store.EPGServiceUpsert{ruleQueryService("default")}); err != nil {
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
		{1001, 0, true, "ニュース7", `[{"lv1":0}]`},
		{1002, time.Hour, true, "アニメスペシャル", `[{"lv1":7}]`},
		{1003, 2 * time.Hour, false, "映画", `[{"lv1":6}]`},
	}
	programs := make([]store.EPGProgramUpsert, 0, len(inserts))
	for _, row := range inserts {
		programs = append(programs, ruleQueryProgram("default", row.id, int32(row.id), start.Add(row.off), row.free, row.name, json.RawMessage(row.g)))
	}
	if err := f.Sync.UpsertPrograms(ctx, programs); err != nil {
		t.Fatal(err)
	}

	// keyword ニュース
	matches, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{
		Sites:       []string{"default"},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := programMatchIDs(matches)
	if len(ids) != 1 || ids[0] != 1001 {
		t.Fatalf("keyword match = %v, want [1001]", ids)
	}
	if got := matches[0]; got.Site != "default" || got.NetworkID != 32736 || got.ServiceID != 1024 ||
		!got.StartAt.Equal(start) || got.DurationMs != 1_800_000 || got.Name != "ニュース7" || !got.IsFree {
		t.Fatalf("keyword match fields = %+v, want the matching program projection", got)
	}

	// genre 7
	matches, err = f.Search.SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default"}, Genres: []int16{7}})
	if err != nil {
		t.Fatal(err)
	}
	ids = programMatchIDs(matches)
	if len(ids) != 1 || ids[0] != 1002 {
		t.Fatalf("genre match = %v, want [1002]", ids)
	}

	// is_free false
	isFree := false
	matches, err = f.Search.SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default"}, IsFree: &isFree})
	if err != nil {
		t.Fatal(err)
	}
	ids = programMatchIDs(matches)
	if len(ids) != 1 || ids[0] != 1003 {
		t.Fatalf("is_free match = %v, want [1003]", ids)
	}

	// channel type GR via join
	matches, err = f.Search.SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default"}, ChannelTypes: []string{"GR"}})
	if err != nil {
		t.Fatal(err)
	}
	ids = programMatchIDs(matches)
	if len(ids) != 3 {
		t.Fatalf("channel type GR = %v, want 3 programs", ids)
	}

	// BS → none
	matches, err = f.Search.SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default"}, ChannelTypes: []string{"BS"}})
	if err != nil {
		t.Fatal(err)
	}
	ids = programMatchIDs(matches)
	if len(ids) != 0 {
		t.Fatalf("channel type BS = %v, want empty", ids)
	}

	// same conditions twice → identical sets (検索と ruler の一致の核)
	c := rulequery.Conditions{
		Sites:       []string{"default"},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "アニメ"}},
		Genres:      []int16{7},
	}
	a, err := f.Search.SearchPrograms(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Search.SearchPrograms(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || (len(a) == 1 && a[0] != b[0]) {
		t.Fatalf("idempotent match diverged: %v vs %v", a, b)
	}

	// 全角キーワードが半角番組名にマッチする（normalize_search_text）
	if err := f.Sync.UpsertPrograms(ctx, []store.EPGProgramUpsert{
		ruleQueryProgram("default", 1004, 4, start.Add(3*time.Hour), true, "NHKニュース", nil),
	}); err != nil {
		t.Fatal(err)
	}
	matches, err = f.Search.SearchPrograms(ctx, rulequery.Conditions{
		Sites:       []string{"default"},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ＮＨＫ"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = programMatchIDs(matches)
	if len(ids) != 1 || ids[0] != 1004 {
		t.Fatalf("fullwidth keyword match = %v, want [1004]", ids)
	}
}

func runEPGKeywordLiterals(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	allIDs := make([]int64, 0, 15)
	idsByName := make(map[string]int64)
	seed := func(name string) int64 {
		id := int64(8200 + len(allIDs))
		seedRuleQueryProgram(t, f, ctx, "default", id, start, name)
		allIDs = append(allIDs, id)
		idsByName[name] = id
		return id
	}

	cases := []struct {
		name  string
		value string
		match string
		decoy string
	}{
		{name: "percent", value: "100%", match: "番組100%編", decoy: "番組100x編"},
		{name: "underscore", value: "NHK_100", match: "番組NHK_100編", decoy: "番組NHKX100編"},
		{name: "backslash", value: `A\B`, match: `番組A\B編`, decoy: "番組AB編"},
		{name: "trailing backslash", value: `Q\`, match: `番組Q\編`, decoy: "番組Q%編"},
		{name: "backslash percent", value: `\%`, match: `番組\%編`, decoy: "番組%編"},
		{name: "backslash underscore", value: `\_`, match: `番組\_編`, decoy: "番組_編"},
	}
	for _, tc := range cases {
		seed(tc.match)
		seed(tc.decoy)
	}
	normalizedMatch := seed("番組NHK_200%編")
	seed("番組NHKX200a編")

	for _, tc := range cases {
		for _, caseSensitive := range []bool{false, true} {
			matches, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{
				Sites: []string{"default"},
				TextMatches: []rulequery.TextMatch{{
					Target: "name", Mode: "keyword", Value: tc.value, CaseSensitive: caseSensitive,
				}},
			})
			if err != nil {
				t.Fatalf("%s caseSensitive=%t: %v", tc.name, caseSensitive, err)
			}
			got := programMatchIDs(matches)
			want := idsByName[tc.match]
			if len(got) != 1 || got[0] != want {
				t.Errorf("%s caseSensitive=%t matched ids %v, want only %d (%q)", tc.name, caseSensitive, got, want, tc.match)
			}
		}
	}

	// 正規化と LIKE エスケープを組み合わせても、全角英字と記号は意図した文字列に一致する。
	normalized, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{
		Sites:       []string{"default"},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ＮＨＫ_200%"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := programMatchIDs(normalized); len(got) != 1 || got[0] != normalizedMatch {
		t.Fatalf("normalized special-character match = %v, want only %d", got, normalizedMatch)
	}

	// Negate はエスケープ後の述語を反転し、対象だけを除外する。
	negated, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{
		Sites:       []string{"default"},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: `A\B`, Negate: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantNegated := make(map[int64]bool, len(allIDs)-1)
	for _, id := range allIDs {
		if id != idsByName[`番組A\B編`] {
			wantNegated[id] = true
		}
	}
	gotNegated := make(map[int64]bool, len(negated))
	for _, match := range negated {
		gotNegated[match.ProgramID] = true
	}
	if len(gotNegated) != len(wantNegated) {
		t.Fatalf("negated match ids = %v, want %d rows", programMatchIDs(negated), len(wantNegated))
	}
	for id := range wantNegated {
		if !gotNegated[id] {
			t.Errorf("negated match ids %v missing %d", programMatchIDs(negated), id)
		}
	}
}

// runEPGRuleSiteGate は rule_sites が非空のとき、
// 対象外の site を評価すると（Compile まで進まず）クエリを投げずに空を返すことを
// 確認する。ruler は site ごとのループで MatchProgramIDsForRule を呼ぶため
// （docs/recording/ruler.md「サイトの扱い」）、対象内の site では通常どおりマッチする
// ことも合わせて見る（両方向。テスト規律）。
func runEPGRuleSiteGate(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()

	start := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	const programID int64 = 9001
	for _, site := range []string{"tokyo", "osaka"} {
		seedRuleQueryProgram(t, f, ctx, site, programID, start, "テスト番組")
	}

	ruleID := f.CreateRule(t, EPGRuleOptions{Name: "rule_sites test", Sites: []string{"tokyo"}})

	// rule_sites = {tokyo} なので osaka は対象外 --- 空を返す。
	ids, err := f.Search.MatchProgramIDsForRule(ctx, "osaka", ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("osaka (対象外) ids = %v, want empty", ids)
	}

	// tokyo は対象内 --- 通常どおりマッチする。
	ids, err = f.Search.MatchProgramIDsForRule(ctx, "tokyo", ruleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != programID {
		t.Fatalf("tokyo (対象内) ids = %v, want [%d]", ids, programID)
	}
}

// runEPGSearchSites は Sites が実際に epg_programs.site で行を絞ることを、
// 返ってきた行そのもので主張する（#530 の罠: 旧テストは WHERE 文字列に "ANY" が
// 含まれるかしか見ておらず、列を参照しない定数述語でも同じように通っていた）。
//
// 2 site にまたがって同一 programId の行を用意し、Sites に 1 site だけを指定すると
// その site の行だけが返ることを見る。p.site 述語を落とす壊し方（例えば
// "TRUE" に縮退させる）をすると、指定していない site の行も混ざって
// 返ってくるためこのテストが落ちる。
func runEPGSearchSites(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()

	start := time.Date(2026, 8, 10, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	const programID int64 = 8001
	for _, site := range []string{"default", "cabin"} {
		seedRuleQueryProgram(t, f, ctx, site, programID, start, "テスト番組")
	}

	matches, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Site != "default" || matches[0].ProgramID != programID {
		t.Fatalf("matches = %+v, want exactly [{default %d}] (cabin の行が混ざってはいけない)", matches, programID)
	}

	// 逆方向も見る（両方向で確認する。テスト規律）: Sites に両方を指定すれば両方返る。
	both, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{Sites: []string{"default", "cabin"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Fatalf("both = %+v, want 2 rows (default + cabin)", both)
	}
}

// allConditionShapes は Compile が生成しうる節をすべて 1 度は含む条件の集合。
// 新しい条件次元を足したらここにも足す。
func allConditionShapes() map[string]rulequery.Conditions {
	free := true
	minMs := int64(60_000)
	maxMs := int64(7_200_000)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)

	return map[string]rulequery.Conditions{
		// Sites は Compile が非空を要求する（#530）ので、他の条件次元だけを見たい
		// ケースにも常に埋めておく。
		"empty":              {Sites: []string{"default"}},
		"sites":              {Sites: []string{"default", "other"}},
		"isFree":             {Sites: []string{"default"}, IsFree: &free},
		"duration":           {Sites: []string{"default"}, DurationMinMs: &minMs, DurationMaxMs: &maxMs},
		"period":             {Sites: []string{"default"}, PeriodStartAt: &start, PeriodEndAt: &end},
		"genres":             {Sites: []string{"default"}, Genres: []int16{0, 7, 15}},
		"services":           {Sites: []string{"default"}, Services: []rulequery.ServiceRef{{NetworkID: 32736, ServiceID: 1024}, {NetworkID: 4, ServiceID: 101}}},
		"channelTypes":       {Sites: []string{"default"}, ChannelTypes: []string{"GR", "BS"}},
		"keyword":            {Sites: []string{"default"}, TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}}},
		"keywordCS":          {Sites: []string{"default"}, TextMatches: []rulequery.TextMatch{{Target: "description", Mode: "keyword", Value: "News", CaseSensitive: true}}},
		"keywordNegate":      {Sites: []string{"default"}, TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "再放送", Negate: true}}},
		"regex":              {Sites: []string{"default"}, TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "regex", Value: "^第[0-9]+話"}}},
		"regexCS":            {Sites: []string{"default"}, TextMatches: []rulequery.TextMatch{{Target: "extended", Mode: "regex", Value: "NHK", CaseSensitive: true}}},
		"times":              {Sites: []string{"default"}, Times: []rulequery.TimeWindow{{Weekdays: 0b0011111, StartSec: 6 * 3600, EndSec: 9 * 3600}}},
		"timesOvernight":     {Sites: []string{"default"}, Times: []rulequery.TimeWindow{{Weekdays: 0b1111111, StartSec: 23 * 3600, EndSec: 1 * 3600}}},
		"timesMultiple":      {Sites: []string{"default"}, Times: []rulequery.TimeWindow{{Weekdays: 1, StartSec: 0, EndSec: 3600}, {Weekdays: 64, StartSec: 3600, EndSec: 86400}}},
		"timesFullDayBounds": {Sites: []string{"default"}, Times: []rulequery.TimeWindow{{Weekdays: 127, StartSec: 0, EndSec: 86400}}},
		"everything": {
			IsFree:        &free,
			DurationMinMs: &minMs,
			DurationMaxMs: &maxMs,
			PeriodStartAt: &start,
			PeriodEndAt:   &end,
			Genres:        []int16{7},
			Services:      []rulequery.ServiceRef{{NetworkID: 32736, ServiceID: 1024}},
			ChannelTypes:  []string{"GR"},
			Sites:         []string{"default"},
			TextMatches: []rulequery.TextMatch{
				{Target: "name", Mode: "keyword", Value: "アニメ"},
				{Target: "description", Mode: "regex", Value: "[0-9]+", Negate: true},
			},
			Times: []rulequery.TimeWindow{{Weekdays: 0b1000000, StartSec: 20 * 3600, EndSec: 22 * 3600}},
		},
	}
}

// runEPGSearchConditionShapes は全条件形のクエリが実 DB に受理されることを確かめる。
// 文字列検査だけのテストでは構文エラーを検出できず、実際に AT TIME ZONE の連結ミスがこの穴をすり抜けた。
func runEPGSearchConditionShapes(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	for name, conditions := range allConditionShapes() {
		t.Run(name, func(t *testing.T) {
			// 実行して初めて構文・型の誤りが出る。行が返るかは問わない。
			if _, err := f.Search.SearchPrograms(ctx, conditions); err != nil {
				t.Fatalf("database rejected generated query: %v", err)
			}
		})
	}
}

func runEPGSearchRequiresSites(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	if _, err := f.Search.SearchPrograms(context.Background(), rulequery.Conditions{}); err == nil {
		t.Fatal("SearchPrograms accepted missing sites")
	}
}

// runEPGRejectsUnknownIdentifiers は列名・演算子が whitelist であることを確かめる。
// ここが緩むと注入経路が開くので、default が error であることをテストで固定する。
func runEPGRejectsUnknownIdentifiers(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	cases := []rulequery.TextMatch{
		{Target: "name; DROP TABLE epg_programs", Mode: "keyword", Value: "x"},
		{Target: "name", Mode: "keyword; DROP TABLE epg_programs", Value: "x"},
		{Target: "", Mode: "keyword", Value: "x"},
		{Target: "name", Mode: "", Value: "x"},
	}
	for _, m := range cases {
		if _, err := f.Search.SearchPrograms(context.Background(), rulequery.Conditions{Sites: []string{"default"}, TextMatches: []rulequery.TextMatch{m}}); err == nil {
			t.Errorf("SearchPrograms accepted unknown identifier: target=%q mode=%q", m.Target, m.Mode)
		}
	}
}
