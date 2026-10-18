package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

	CreateRule func(t *testing.T, options EPGRuleOptions) int64
	MarkStale  func(t *testing.T, site string, serviceIDs []int32, programIDs []int64)
}

// NewEPGFixture は実在する EPG 契約実装と状態作成用フックを作る。
type NewEPGFixture func(t *testing.T) EPGFixture

// RunEPG は実在する EPG 契約実装に同期・読み取り・検索の契約を実行する。
func RunEPG(t *testing.T, newFixture NewEPGFixture) {
	t.Helper()
	t.Run("sync writes one batch and readers return the projection", func(t *testing.T) {
		runEPGSyncAndRead(t, newFixture)
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
	if err := f.Sync.UpsertPrograms(ctx, programs); err != nil {
		t.Fatalf("upserting programs: %v", err)
	}

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
	if detail.Program.ProgramID != 1001 || detail.Program.GenreLv1[0] != 7 {
		t.Fatalf("program detail = %+v, want program 1001 with genre 7", detail)
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
	if err := f.Search.ValidateRegex(context.Background(), "^(NHK|ニュース)$"); err != nil {
		t.Fatalf("ValidateRegex(valid) = %v", err)
	}
	if err := f.Search.ValidateRegex(context.Background(), "(?<name>news)"); !errors.Is(err, store.ErrInvalidPattern) {
		t.Fatalf("ValidateRegex(invalid) = %v, want ErrInvalidPattern", err)
	}
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

func programMatchIDs(matches []store.ProgramMatch) []int64 {
	ids := make([]int64, len(matches))
	for i, match := range matches {
		ids[i] = match.ProgramID
	}
	return ids
}
