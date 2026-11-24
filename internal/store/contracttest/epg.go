package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/store"
)

// EPGRuleOptions は番組評価で使う親条件と子テーブル条件である。
type EPGRuleOptions struct {
	Name          string
	IsFree        *bool
	DurationMinMs *int64
	DurationMaxMs *int64
	TextMatches   []rulequery.TextMatch
	Services      []rulequery.ServiceRef
	ChannelTypes  []string
	Genres        []int16
	Times         []rulequery.TimeWindow
	Sites         []string
}

// EPGFixture は EPG の 3 契約と実装固有の状態作成フックをまとめる。
type EPGFixture struct {
	Sync   store.EPGSync
	Reader store.EPGReader
	Search store.ProgramSearch

	// ValidRegexPattern is accepted by this implementation's regex syntax.
	ValidRegexPattern string
	// InvalidRegexPattern is rejected by this implementation's regex syntax.
	InvalidRegexPattern string

	CreateRule          func(t *testing.T, options EPGRuleOptions) int64
	MarkStale           func(t *testing.T, site string, serviceIDs []int32, programIDs []int64)
	ObserveNotification func(t *testing.T, notify func()) string
	CreateRecording     func(t *testing.T, program store.EPGProgramUpsert, status string, labelRuleID *int64) int64
	CreateSeriesLabel   func(t *testing.T, keyword, value string, priority int) int64
	SetIntent           func(t *testing.T, site string, programID int64, action string)
}

// NewEPGFixture は実在する EPG 契約実装と状態作成用フックを作る。
type NewEPGFixture func(t *testing.T) EPGFixture

// RunEPG は実在する EPG 契約実装に同期・読み取り・検索の契約を実行する。
func RunEPG(t *testing.T, newFixture NewEPGFixture) {
	t.Helper()
	t.Run("sync writes one batch and readers return the projection", func(t *testing.T) {
		runEPGSyncAndRead(t, newFixture)
	})
	t.Run("sync writes and refreshes projected genre levels", func(t *testing.T) {
		runEPGSyncGenreLevels(t, newFixture)
	})
	t.Run("keyword search normalizes width and respects case sensitivity", func(t *testing.T) {
		runEPGKeywordSearch(t, newFixture)
	})
	t.Run("rule matching applies every child-table condition", func(t *testing.T) {
		runEPGRuleConditions(t, newFixture)
	})
	t.Run("regex validation distinguishes syntax errors", func(t *testing.T) {
		runEPGRegexValidation(t, newFixture)
	})
	t.Run("search applies keyword genre free and channel filters", func(t *testing.T) {
		runEPGSearchIntegration(t, newFixture)
	})
	t.Run("keyword LIKE special characters are literal", func(t *testing.T) {
		runEPGKeywordLiterals(t, newFixture)
	})
	t.Run("rule sites gate the evaluation site", func(t *testing.T) {
		runEPGRuleSiteGate(t, newFixture)
	})
	t.Run("search filters by site without collapsing duplicate program IDs", func(t *testing.T) {
		runEPGSearchSites(t, newFixture)
	})
	t.Run("search accepts every supported condition shape", func(t *testing.T) {
		runEPGSearchConditionShapes(t, newFixture)
	})
	t.Run("search requires sites", func(t *testing.T) {
		runEPGSearchRequiresSites(t, newFixture)
	})
	t.Run("search rejects unknown text identifiers", func(t *testing.T) {
		runEPGRejectsUnknownIdentifiers(t, newFixture)
	})
	t.Run("explicit notification publishes the EPG update", func(t *testing.T) {
		runEPGNotify(t, newFixture)
	})
	t.Run("upcoming programs follow the recording effective series", func(t *testing.T) {
		runEPGUpcomingSeries(t, newFixture)
	})
}

func runEPGSyncAndRead(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 19, 9, 30, 0, 0, time.FixedZone("JST", 9*60*60))
	services := []store.EPGServiceUpsert{
		epgService("default", 1, "局 A", "GR"),
		epgService("default", 2, "局 B", "BS"),
	}
	if err := f.Sync.UpsertServices(ctx, services); err != nil {
		t.Fatalf("upserting services: %v", err)
	}
	programs := []store.EPGProgramUpsert{
		epgProgram("default", 1001, 1, "ＮＨＫニュース", "説明", start, 1_800_000, 7),
		epgProgram("default", 1002, 2, "別の番組", "説明", start.Add(time.Hour), 1_800_000, 6),
	}
	programs[0].Genres = json.RawMessage(`[{"lv1":7},{"lv1":"03"},{"lv1":7},{"lv1":16},{"lv1":1.5},{"LV1":1},{}]`)
	if err := f.Sync.UpsertPrograms(ctx, programs); err != nil {
		t.Fatalf("upserting programs: %v", err)
	}

	f.SetIntent(t, "default", 1001, "record")
	recordingID := f.CreateRecording(t, programs[0], "recording", nil)

	listedServices, err := f.Reader.ListServices(ctx, "default")
	if err != nil {
		t.Fatalf("listing services: %v", err)
	}
	if len(listedServices) != 2 || !listedServices[0].HasPrograms || !listedServices[1].HasPrograms {
		t.Fatalf("services = %+v, want both services with programs", listedServices)
	}
	listQuery := store.EPGProgramListQuery{
		Site:        "default",
		WindowStart: start.Add(-time.Minute),
		WindowEnd:   start.Add(2 * time.Hour),
	}
	listedPrograms, err := f.Reader.ListPrograms(ctx, listQuery)
	if err != nil {
		t.Fatalf("listing programs: %v", err)
	}
	if len(listedPrograms) != 2 {
		t.Fatalf("programs = %+v, want 2 rows", listedPrograms)
	}
	if got := listedPrograms[0]; got.IntentAction == nil || *got.IntentAction != "record" ||
		got.RecordingID == nil || *got.RecordingID != recordingID {
		t.Fatalf("program list item = %+v, want record intent and active recording %d", got, recordingID)
	}
	if got := listedPrograms[0].GenreLv1; !slices.Equal(got, []int16{3, 7}) {
		t.Fatalf("program list genres = %v, want [3 7]", got)
	}
	if got := listedPrograms[1]; got.IntentAction != nil || got.RecordingID != nil {
		t.Fatalf("program list item = %+v, want no intent or recording", got)
	}
	listQuery.Services = []store.EPGServiceRef{{NetworkID: 1, ServiceID: 1}}
	filtered, err := f.Reader.ListPrograms(ctx, listQuery)
	if err != nil {
		t.Fatalf("listing programs by service: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ProgramID != 1001 {
		t.Fatalf("service-filtered programs = %+v, want program 1001", filtered)
	}

	detail, err := f.Reader.GetProgram(ctx, "default", 1001)
	if err != nil {
		t.Fatalf("getting program detail: %v", err)
	}
	if detail.Program.ProgramID != 1001 || !slices.Equal(detail.Program.GenreLv1, []int16{3, 7}) {
		t.Fatalf("program detail = %+v, want program 1001 with genres [3 7]", detail)
	}
	if detail.IntentAction == nil || *detail.IntentAction != "record" {
		t.Fatalf("program detail intent = %v, want record", detail.IntentAction)
	}
	var extended map[string]string
	if err := json.Unmarshal(detail.Program.Extended, &extended); err != nil || extended["key"] != "value" {
		t.Fatalf("extended = %s, error = %v, want {\"key\":\"value\"}", detail.Program.Extended, err)
	}
	if _, err := f.Reader.GetProgram(ctx, "default", -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetProgram(missing) error = %v, want ErrNotFound", err)
	}

	f.MarkStale(t, "default", []int32{2}, []int64{1002})
	mark, err := f.Sync.SweepMark(ctx)
	if err != nil {
		t.Fatalf("getting sweep mark: %v", err)
	}
	if err := f.Sync.UpsertServices(ctx, []store.EPGServiceUpsert{epgService("default", 1, "局 A 更新", "GR")}); err != nil {
		t.Fatalf("refreshing service: %v", err)
	}
	if err := f.Sync.UpsertPrograms(ctx, []store.EPGProgramUpsert{programs[0]}); err != nil {
		t.Fatalf("refreshing program: %v", err)
	}
	deletedServices, err := f.Sync.DeleteStaleServices(ctx, "default", mark)
	if err != nil {
		t.Fatalf("deleting stale services: %v", err)
	}
	if deletedServices != 1 {
		t.Fatalf("stale services deleted = %d, want 1", deletedServices)
	}
	deletedPrograms, err := f.Sync.DeleteStalePrograms(ctx, "default", mark, 1, []int32{1, 2})
	if err != nil {
		t.Fatalf("deleting stale programs: %v", err)
	}
	if deletedPrograms != 1 {
		t.Fatalf("stale programs deleted = %d, want 1", deletedPrograms)
	}
	if _, err := f.Reader.GetProgram(ctx, "default", 1002); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetProgram(stale) error = %v, want ErrNotFound", err)
	}
	servicesAfterSweep, err := f.Reader.ListServices(ctx, "default")
	if err != nil {
		t.Fatalf("listing services after sweep: %v", err)
	}
	if len(servicesAfterSweep) != 1 || servicesAfterSweep[0].Name != "局 A 更新" {
		t.Fatalf("services after sweep = %+v, want refreshed service only", servicesAfterSweep)
	}

	oldProgram := epgProgram("default", 1003, 1, "終了番組", "", start.Add(-48*time.Hour), int64(time.Hour/time.Millisecond), 7)
	if err := f.Sync.UpsertPrograms(ctx, []store.EPGProgramUpsert{oldProgram}); err != nil {
		t.Fatalf("upserting aired program: %v", err)
	}
	pruned, err := f.Sync.PrunePrograms(ctx, "default", start.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("pruning aired programs: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned programs = %d, want 1", pruned)
	}
}

// runEPGSyncGenreLevels は genre_lv1_of と同じ規則で同期時に大分類が更新されることを確かめる。
func runEPGSyncGenreLevels(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.Sync.UpsertServices(ctx, []store.EPGServiceUpsert{epgService("default", 1, "局 A", "GR")}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	program := epgProgram("default", 1001, 1, "番組", "", start, 1_800_000, 7)

	// 同じ番組を更新する。空にするケースの直前には大分類を入れておく。
	cases := []struct {
		name   string
		genres json.RawMessage
		want   []int16
	}{
		{"insert sorts and deduplicates levels", json.RawMessage(`[{"lv1":7},{"lv1":"03"},{"lv1":7}]`), []int16{3, 7}},
		{"update replaces old levels with expanded exponents", json.RawMessage(`[{"lv1":1e1},{"lv1":1.5e1}]`), []int16{10, 15}},
		{"fractional exponent clears levels", json.RawMessage(`[{"lv1":7e-1}]`), nil},
		{"negative zero becomes zero", json.RawMessage(`[{"lv1":-0}]`), []int16{0}},
		{"decimal spelling is excluded even with zero fractional digits", json.RawMessage(`[{"lv1":7.0}]`), nil},
		{"negative zero with an exponent becomes zero", json.RawMessage(`[{"lv1":-0.0e1}]`), []int16{0}},
		{"SQL NULL clears levels", nil, nil},
		{"write a level before the JSON null", json.RawMessage(`[{"lv1":4}]`), []int16{4}},
		{"JSON null clears levels", json.RawMessage(`null`), nil},
		{"write a level before the JSON object", json.RawMessage(`[{"lv1":6}]`), []int16{6}},
		{"a non-array JSON object clears levels", json.RawMessage(`{"lv1":12}`), nil},
		{"out of range numbers are excluded", json.RawMessage(`[{"lv1":16},{"lv1":-1},{"lv1":1e2},{"lv1":100}]`), nil},
		{"non-digit strings and non-numeric values are excluded", json.RawMessage(`[{"lv1":"news"},{"lv1":"1e1"},{"lv1":true},{"lv1":null},{"lv1":[4]},4,{"LV1":8},{}]`), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			program.Genres = tc.genres
			if err := f.Sync.UpsertPrograms(ctx, []store.EPGProgramUpsert{program}); err != nil {
				t.Fatalf("upserting genres %q: %v", tc.genres, err)
			}
			detail, err := f.Reader.GetProgram(ctx, program.Site, program.ProgramID)
			if err != nil {
				t.Fatalf("reading program: %v", err)
			}
			if got := detail.Program.GenreLv1; !slices.Equal(got, tc.want) {
				t.Fatalf("genre levels for %q = %v, want %v", tc.genres, got, tc.want)
			}
		})
	}
}

func runEPGKeywordSearch(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 19, 9, 30, 0, 0, time.FixedZone("JST", 9*60*60))
	if err := f.Sync.UpsertServices(ctx, []store.EPGServiceUpsert{epgService("default", 1, "局", "GR")}); err != nil {
		t.Fatalf("upserting service: %v", err)
	}
	programs := []store.EPGProgramUpsert{
		epgProgram("default", 1101, 1, "ＮＨＫニュース", "", start, 1_800_000, 7),
		epgProgram("default", 1102, 1, "NHKニュース", "", start.Add(time.Hour), 1_800_000, 7),
	}
	if err := f.Sync.UpsertPrograms(ctx, programs); err != nil {
		t.Fatalf("upserting programs: %v", err)
	}
	search := func(value string, caseSensitive bool) []store.ProgramMatch {
		t.Helper()
		matches, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{
			Sites: []string{"default"},
			TextMatches: []rulequery.TextMatch{{
				Target: "name", Mode: "keyword", Value: value, CaseSensitive: caseSensitive,
			}},
		})
		if err != nil {
			t.Fatalf("searching keyword %q (caseSensitive=%t): %v", value, caseSensitive, err)
		}
		return matches
	}
	widthInsensitive := search("ＮＨＫ", false)
	if got := programMatchIDs(widthInsensitive); !reflect.DeepEqual(got, []int64{1101, 1102}) {
		t.Fatalf("full-width case-insensitive matches = %v, want [1101 1102]", got)
	}
	caseInsensitive := search("nhk", false)
	if got := programMatchIDs(caseInsensitive); !reflect.DeepEqual(got, []int64{1101, 1102}) {
		t.Fatalf("lower-case case-insensitive matches = %v, want [1101 1102]", got)
	}
	caseSensitive := search("NHK", true)
	if got := programMatchIDs(caseSensitive); !reflect.DeepEqual(got, []int64{1102}) {
		t.Fatalf("case-sensitive matches = %v, want [1102]", got)
	}
	caseSensitiveMiss := search("nhk", true)
	if len(caseSensitiveMiss) != 0 {
		t.Fatalf("lower-case case-sensitive matches = %v, want none", programMatchIDs(caseSensitiveMiss))
	}

	// 正規表現の構文は実装依存だが、先頭一致と英字の大文字小文字の指定は揃える。
	// 当たるべき番組は 1102 を含むかで判定し、全角の 1101 には何も言わない。
	searchRegex := func(value string, caseSensitive bool) []int64 {
		t.Helper()
		matches, err := f.Search.SearchPrograms(ctx, rulequery.Conditions{
			Sites: []string{"default"},
			TextMatches: []rulequery.TextMatch{{
				Target: "name", Mode: "regex", Value: value, CaseSensitive: caseSensitive,
			}},
		})
		if err != nil {
			t.Fatalf("searching regex %q (caseSensitive=%t): %v", value, caseSensitive, err)
		}
		return programMatchIDs(matches)
	}
	if got := searchRegex("^nhk", false); !slices.Contains(got, 1102) {
		t.Fatalf("case-insensitive regex ^nhk matches = %v, want to contain 1102", got)
	}
	if got := searchRegex("^NHK", true); !slices.Contains(got, 1102) {
		t.Fatalf("case-sensitive regex ^NHK matches = %v, want to contain 1102", got)
	}
	if got := searchRegex("^nhk", true); len(got) != 0 {
		t.Fatalf("case-sensitive regex ^nhk matches = %v, want none", got)
	}
}

func runEPGRuleConditions(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 19, 9, 30, 0, 0, time.FixedZone("JST", 9*60*60))
	services := []store.EPGServiceUpsert{
		epgService("default", 1, "地上波", "GR"),
		epgService("default", 2, "BS", "BS"),
		epgService("default", 3, "別サービス", "GR"),
		epgService("other", 1, "別 site", "GR"),
	}
	if err := f.Sync.UpsertServices(ctx, services); err != nil {
		t.Fatalf("upserting services: %v", err)
	}
	programs := []store.EPGProgramUpsert{
		epgProgram("default", 2001, 1, "needle", "special", start, 1_800_000, 7),
		epgProgram("default", 2002, 1, "not included", "special", start, 1_800_000, 7),
		epgProgram("default", 2003, 3, "needle", "special", start, 1_800_000, 7),
		epgProgram("default", 2004, 2, "needle", "special", start, 1_800_000, 7),
		epgProgram("default", 2005, 1, "needle", "special", start, 1_800_000, 6),
		epgProgram("default", 2006, 1, "needle", "special", start.Add(-48*time.Hour), 1_800_000, 7),
		epgProgram("default", 2007, 1, "needle", "ordinary", start, 1_800_000, 7),
		epgProgram("default", 2008, 1, "needle", "special", start, 1_800_000, 7),
		epgProgram("default", 2009, 1, "needle", "special", start, 3_000_000, 7),
		epgProgram("other", 2010, 1, "needle", "special", start, 1_800_000, 7),
		// 時刻帯の外（同じ月曜の 11:30）。曜日だけで判定すると当たってしまう。
		epgProgram("default", 2011, 1, "needle", "special", start.Add(2*time.Hour), 1_800_000, 7),
		// 最短未満の番組長。DurationMinMs を無視すると当たってしまう。
		epgProgram("default", 2012, 1, "needle", "special", start, 500_000, 7),
		// 日跨ぎの窓（End < Start）用。曜日は番組開始日の曜日で判定する。
		epgProgram("default", 2013, 1, "overnight", "", start.Add(14*time.Hour), 1_800_000, 7),
		epgProgram("default", 2015, 1, "overnight", "", start.Add(2*time.Hour+30*time.Minute), 1_800_000, 7),
		epgProgram("default", 2016, 1, "overnight", "", start.Add(-9*time.Hour), 1_800_000, 7),
		epgProgram("default", 2017, 1, "overnight", "", start.Add(15*time.Hour), 1_800_000, 7),
	}
	programs[7].IsFree = false
	if err := f.Sync.UpsertPrograms(ctx, programs); err != nil {
		t.Fatalf("upserting programs: %v", err)
	}
	isFree := true
	minDuration, maxDuration := int64(1_000_000), int64(2_000_000)
	ruleID := f.CreateRule(t, EPGRuleOptions{
		Name:          "all EPG child filters",
		IsFree:        &isFree,
		DurationMinMs: &minDuration,
		DurationMaxMs: &maxDuration,
		TextMatches: []rulequery.TextMatch{
			{Target: "name", Mode: "keyword", Value: "needle"},
			{Target: "description", Mode: "regex", Value: "^special$"},
		},
		Services:     []rulequery.ServiceRef{{NetworkID: 1, ServiceID: 1}, {NetworkID: 1, ServiceID: 2}},
		ChannelTypes: []string{"GR"},
		Genres:       []int16{7},
		Times:        []rulequery.TimeWindow{{Weekdays: 1, StartSec: 9 * 3600, EndSec: 10 * 3600}},
		Sites:        []string{"default"},
	})
	ids, err := f.Search.MatchProgramIDsForRule(ctx, "default", ruleID)
	if err != nil {
		t.Fatalf("matching rule: %v", err)
	}
	if !reflect.DeepEqual(ids, []int64{2001}) {
		t.Fatalf("matching program IDs = %v, want [2001] after applying every child condition", ids)
	}
	overnightID := f.CreateRule(t, EPGRuleOptions{
		Name:  "overnight window",
		Times: []rulequery.TimeWindow{{Weekdays: 1, StartSec: 23 * 3600, EndSec: 1 * 3600}},
		Sites: []string{"default"},
	})
	overnight, err := f.Search.MatchProgramIDsForRule(ctx, "default", overnightID)
	if err != nil {
		t.Fatalf("matching overnight rule: %v", err)
	}
	slices.Sort(overnight)
	if !reflect.DeepEqual(overnight, []int64{2013, 2016}) {
		t.Fatalf("overnight window program IDs = %v, want [2013 2016] (Mon 23:30 and Mon 00:30 in; Tue 00:30 and Mon 12:00 out)", overnight)
	}
	outOfScope, err := f.Search.MatchProgramIDsForRule(ctx, "other", ruleID)
	if err != nil {
		t.Fatalf("matching rule outside its sites: %v", err)
	}
	if len(outOfScope) != 0 {
		t.Fatalf("program IDs outside rule sites = %v, want none", outOfScope)
	}
}

func runEPGRegexValidation(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	if err := f.Search.ValidateRegex(context.Background(), f.ValidRegexPattern); err != nil {
		t.Fatalf("ValidateRegex(valid) = %v", err)
	}
	if err := f.Search.ValidateRegex(context.Background(), f.InvalidRegexPattern); !errors.Is(err, store.ErrInvalidPattern) {
		t.Fatalf("ValidateRegex(invalid) = %v, want ErrInvalidPattern", err)
	}
}

func runEPGNotify(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	topic := f.ObserveNotification(t, func() {
		if err := f.Sync.NotifyProgramsUpdated(context.Background()); err != nil {
			t.Fatalf("notifying programs updated: %v", err)
		}
	})
	if topic != "epg" {
		t.Fatalf("notification topic = %q, want epg", topic)
	}
}

func runEPGUpcomingSeries(t *testing.T, newFixture NewEPGFixture) {
	f := newFixture(t)
	ctx := context.Background()
	mark, err := f.Sync.SweepMark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := mark.Add(time.Hour)
	origin := epgProgram("default", 3000, 1, "天気 第1話", "", mark.Add(-2*time.Hour), 1_800_000, 0)
	recordingID := f.CreateRecording(t, origin, "finished", nil)
	programs := []store.EPGProgramUpsert{
		epgProgram("default", 3001, 1, "天気 第2話", "", start, 1_800_000, 0),
		epgProgram("other", 3001, 1, "天気 第2話", "", start, 1_800_000, 0),
		epgProgram("default", 3002, 1, "天気 第3話", "", mark.Add(-time.Hour), 1_800_000, 0),
		epgProgram("default", 3003, 1, "別番組 第2話", "", start, 1_800_000, 0),
		epgProgram("default", 3004, 1, "中継 新作", "", start.Add(-time.Minute), 1_800_000, 0),
	}
	if err := f.Sync.UpsertPrograms(ctx, programs); err != nil {
		t.Fatal(err)
	}
	// 番組と意図は別の寿命を持つ。skip した回もシリーズの次回には含まれる。
	f.SetIntent(t, "default", 3001, "skip")
	assertUpcoming := func(recordingID int64, want []string) {
		t.Helper()
		matches, err := f.Reader.ListUpcomingProgramsBySeries(ctx, recordingID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, match := range matches {
			got = append(got, match.Site+"/"+strconv.FormatInt(match.ProgramID, 10))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("upcoming for recording %d = %v, want %v", recordingID, got, want)
		}
	}
	assertUpcoming(recordingID, []string{"default/3001", "other/3001"})
	assertUpcoming(-1, nil)
	empty := origin
	empty.EventID++
	empty.Name = "★"
	assertUpcoming(f.CreateRecording(t, empty, "finished", nil), nil)

	f.CreateSeriesLabel(t, "中継", "別枠", 0)
	labelID := f.CreateSeriesLabel(t, "中継", "天気", 10)
	labeled := origin
	labeled.EventID += 2
	labeled.Name = "中継 過去回"
	assertUpcoming(f.CreateRecording(t, labeled, "finished", &labelID), []string{"default/3004", "default/3001", "other/3001"})
}

func epgService(site string, serviceID int32, name, channelType string) store.EPGServiceUpsert {
	return store.EPGServiceUpsert{
		Site: site, NetworkID: 1, ServiceID: serviceID, Type: 1,
		Name: name, ChannelType: channelType, Channel: "27",
	}
}

func epgProgram(site string, id int64, serviceID int32, name, description string, start time.Time, durationMs int64, genre int16) store.EPGProgramUpsert {
	return store.EPGProgramUpsert{
		Site: site, ProgramID: id, NetworkID: 1, ServiceID: serviceID,
		EventID: int32(id), StartAt: start, DurationMs: durationMs,
		EndAt: start.Add(time.Duration(durationMs) * time.Millisecond), IsFree: true,
		Name: name, Description: description,
		Extended: json.RawMessage(`{"key":"value"}`),
		Genres:   json.RawMessage(`[{"lv1":` + strconv.Itoa(int(genre)) + `}]`),
	}
}

// programMatchIDs は ProgramMatch のスライスから programId だけを取り出す
// （site を無視して集合比較したいテスト向けの小さなヘルパー）。
func programMatchIDs(matches []store.ProgramMatch) []int64 {
	ids := make([]int64, len(matches))
	for i, match := range matches {
		ids[i] = match.ProgramID
	}
	return ids
}
