package epgimport

import (
	"context"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/testutil"
)

func boolPtr(b bool) *bool { return &b }

func basicRule(id int64, keyword string) epgstation.Rule {
	return epgstation.Rule{
		ID: id,
		SearchOption: epgstation.RuleSearchOption{
			Keyword: keyword,
			Name:    true,
			IsFree:  boolPtr(true),
		},
		ReserveOption: epgstation.RuleReserveOption{Enable: true, AllowEndLack: true},
	}
}

// TestImportRules_IdempotentRerun is the acceptance criterion: re-running the
// same import (same EPGStation rule id) must not add rows.
func TestImportRules_IdempotentRerun(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{basicRule(1, "ニュース")}

	first, err := ImportRules(ctx, pool, "default", rules)
	if err != nil {
		t.Fatalf("first ImportRules: %v", err)
	}
	if first.Created != 1 || first.Updated != 0 {
		t.Fatalf("first result = %+v, want Created=1 Updated=0", first)
	}

	second, err := ImportRules(ctx, pool, "default", rules)
	if err != nil {
		t.Fatalf("second ImportRules: %v", err)
	}
	if second.Created != 0 || second.Updated != 1 {
		t.Fatalf("second result = %+v, want Created=0 Updated=1", second)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rules`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rules count = %d, want 1 (re-run must not duplicate)", count)
	}

	var textMatchCount, siteCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rule_text_matches`).Scan(&textMatchCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rule_sites`).Scan(&siteCount); err != nil {
		t.Fatal(err)
	}
	if textMatchCount != 1 || siteCount != 1 {
		t.Fatalf("child rows after rerun: text_matches=%d sites=%d, want 1/1 (delete-then-reinsert must not accumulate)",
			textMatchCount, siteCount)
	}
}

// TestImportRules_AREIncompatibleRegexWarns is the acceptance criterion: an
// ARE-incompatible regex surfaces as a warning, and the offending text match
// is not inserted (so the rule stays evaluable). It also covers blocking
// finding 1 from review: dropping the rule's only condition must not leave
// it enabled (rulequery.Compile degenerates to a bare site filter, which
// would record the entire EPG).
//
// Pattern choice note: this uses JS-style named capture groups,
// (?<name>...), rather than lookbehind — measured directly against this
// task's Postgres 16.2, lookbehind actually works (docs/data/search.md
// documents this), so it would not exercise the ARE-incompatible path.
// Matching an empty string against (?<name>...) raises "quantifier operand
// invalid" instead.
func TestImportRules_AREIncompatibleRegexWarns(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID: 2,
		SearchOption: epgstation.RuleSearchOption{
			Keyword:   "(?<name>foo)bar", // JS named capture group: not POSIX ARE
			KeyRegExp: true,
			Name:      true,
		},
		ReserveOption: epgstation.RuleReserveOption{Enable: true, AllowEndLack: true},
	}}

	result, err := ImportRules(ctx, pool, "default", rules)
	if err != nil {
		t.Fatalf("ImportRules: %v", err)
	}
	if len(result.Warnings) < 2 {
		t.Fatalf("warnings = %+v, want at least 2 (ARE-incompatible regex + no-conditions-left)", result.Warnings)
	}
	for _, w := range result.Warnings {
		if w.EpgstationRuleID != 2 {
			t.Errorf("warning %+v: EpgstationRuleID != 2", w)
		}
	}

	var textMatchCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rule_text_matches`).Scan(&textMatchCount); err != nil {
		t.Fatal(err)
	}
	if textMatchCount != 0 {
		t.Fatalf("rule_text_matches count = %d, want 0 (incompatible regex must not be persisted)", textMatchCount)
	}

	// The acceptance criterion says "surfaces as a warning" — it does not say
	// "and the rule is silently imported as a catch-all". A rule with zero
	// narrowing conditions left must not be enabled.
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM rules WHERE metadata @> '{"epgstation":{"ruleId":2}}'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("enabled = true, want false (rule has no narrowing conditions after the ARE-incompatible match was dropped — must not silently record the entire EPG)")
	}
}

// TestImportRules_NoConditionsFromEPGStation_ImportsDisabled: the same
// no-conditions guard also has to catch rules that never had a narrowing
// condition to begin with (not just ones emptied by conversion loss).
func TestImportRules_NoConditionsFromEPGStation_ImportsDisabled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID:            5,
		SearchOption:  epgstation.RuleSearchOption{}, // no keyword, no services, nothing
		ReserveOption: epgstation.RuleReserveOption{Enable: true},
	}}

	result, err := ImportRules(ctx, pool, "default", rules)
	if err != nil {
		t.Fatalf("ImportRules: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("want a warning about the rule having no narrowing conditions, got none")
	}

	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM rules WHERE metadata @> '{"epgstation":{"ruleId":5}}'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("enabled = true, want false (rule has no narrowing conditions at all)")
	}
}

// TestImportRules_ChildInsertFailureRollsBack is the acceptance criterion
// behind blocking finding 2: writing the rule row and its child tables must
// be all-or-nothing. A blank site fails rule_sites' non-empty-site CHECK
// on the very last insert in writeRuleChildren, after the rule row and every
// other child table already succeeded — without a transaction, those earlier
// writes would stick around as a "some conditions, but not all" rule.
func TestImportRules_ChildInsertFailureRollsBack(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{basicRule(6, "keyword")}

	if _, err := ImportRules(ctx, pool, "", rules); err == nil {
		t.Fatal("want an error from the empty site (rule_sites CHECK violation), got nil")
	}

	var ruleCount, textMatchCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rules`).Scan(&ruleCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rule_text_matches`).Scan(&textMatchCount); err != nil {
		t.Fatal(err)
	}
	if ruleCount != 0 || textMatchCount != 0 {
		t.Fatalf("rules=%d rule_text_matches=%d after a failed import, want 0/0 (partial writes must roll back)",
			ruleCount, textMatchCount)
	}
}

// TestImportRules_IsFreeFalseIsNotAFilter: EPGStation's isFree is a checkbox,
// not a tristate — the DB column is NOT NULL DEFAULT false and RuleDB.ts
// writes back `!!isFree` unconditionally, so GET /api/rules always returns
// "isFree": false for a rule that never ticked the box (it is never
// omitted). Importing that false as a real filter turns an unfiltered rule
// into one that matches only pay/scrambled programs, silently recording
// nothing for the common terrestrial case.
func TestImportRules_IsFreeFalseIsNotAFilter(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID: 8,
		SearchOption: epgstation.RuleSearchOption{
			Keyword: "ニュース", Name: true, IsFree: boolPtr(false),
		},
		ReserveOption: epgstation.RuleReserveOption{Enable: true},
	}}

	if _, err := ImportRules(ctx, pool, "default", rules); err != nil {
		t.Fatalf("ImportRules: %v", err)
	}

	var isFree *bool
	if err := pool.QueryRow(ctx, `SELECT is_free FROM rules WHERE metadata @> '{"epgstation":{"ruleId":8}}'`).Scan(&isFree); err != nil {
		t.Fatal(err)
	}
	if isFree != nil {
		t.Errorf("is_free = %v, want NULL (EPGStation's unticked isFree checkbox must not become a pay-TV-only filter)", *isFree)
	}
}

// TestImportRules_UnsupportedTemplateVariableWarns is the acceptance
// criterion: an unsupported %変数% (e.g. %CHNAME%) must not silently become
// an empty filenameTemplate producing garbled paths — it must warn and fall
// back to the rokuban default template (empty filename_template column).
func TestImportRules_UnsupportedTemplateVariableWarns(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID:            3,
		SearchOption:  epgstation.RuleSearchOption{Keyword: "foo", Name: true},
		ReserveOption: epgstation.RuleReserveOption{Enable: true, AllowEndLack: true},
		SaveOption:    &epgstation.ReserveSaveOption{RecordedFormat: "%CHNAME%_%TITLE%"},
	}}

	result, err := ImportRules(ctx, pool, "default", rules)
	if err != nil {
		t.Fatalf("ImportRules: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("want a warning for unsupported recordedFormat variable, got none")
	}

	var filenameTemplate string
	if err := pool.QueryRow(ctx, `SELECT filename_template FROM rules WHERE metadata @> '{"epgstation":{"ruleId":3}}'`).
		Scan(&filenameTemplate); err != nil {
		t.Fatal(err)
	}
	if filenameTemplate != "" {
		t.Errorf("filename_template = %q, want \"\" (unsupported variable must fall back to DefaultTemplate, not become a broken template)", filenameTemplate)
	}
}

// TestImportRules_TimeSpecifiedSkipped: time-specified rules are the feature
// rokuban dropped wholesale; import must detect and skip them with a warning
// rather than fabricate a programId-less reservation shape.
func TestImportRules_TimeSpecifiedSkipped(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID:                  4,
		IsTimeSpecification: true,
		ReserveOption:       epgstation.RuleReserveOption{Enable: true},
	}}

	result, err := ImportRules(ctx, pool, "default", rules)
	if err != nil {
		t.Fatalf("ImportRules: %v", err)
	}
	if result.Skipped != 1 || result.Created != 0 {
		t.Fatalf("result = %+v, want Skipped=1 Created=0", result)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rules`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rules count = %d, want 0", count)
	}
}

func TestEpgWeekdayToRokuban(t *testing.T) {
	cases := []struct {
		name string
		epg  int
		want int
	}{
		{"sunday only", 0x01, 1 << 6},
		{"monday only", 0x02, 1 << 0},
		{"saturday only", 0x40, 1 << 5},
		{"weekdays mon-fri", 0x02 | 0x04 | 0x08 | 0x10 | 0x20, 0b0011111},
		{"none", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := epgWeekdayToRokuban(c.epg); got != c.want {
				t.Errorf("epgWeekdayToRokuban(%#x) = %#x, want %#x", c.epg, got, c.want)
			}
		})
	}
}

func TestBuildTextMatches_KeywordMultipleTargetsWarnsAndPicksOne(t *testing.T) {
	var warnings []string
	opt := epgstation.RuleSearchOption{Keyword: "foo", Name: true, Description: true}
	got := buildTextMatches(opt, &warnings)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (multi-target positive keyword can't be OR'd, must pick one)", len(got))
	}
	if got[0].Target != "name" {
		t.Errorf("target = %q, want name (priority order)", got[0].Target)
	}
	if len(warnings) == 0 {
		t.Error("want a warning about dropped OR semantics, got none")
	}
}

func TestBuildTextMatches_IgnoreKeywordMultipleTargetsAllKept(t *testing.T) {
	// De Morgan: NOT(A) AND NOT(B) == NOT(A OR B), so ignoreKeyword with
	// multiple target fields can be represented exactly as multiple negated rows.
	var warnings []string
	opt := epgstation.RuleSearchOption{IgnoreKeyword: "bar", IgnoreName: true, IgnoreDescription: true}
	got := buildTextMatches(opt, &warnings)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (De Morgan allows keeping both fields)", len(got))
	}
	for _, m := range got {
		if !m.Negate {
			t.Errorf("match %+v: Negate = false, want true", m)
		}
	}
}

// TestBuildRuleFields_DroppedSubGenreWarns: SubGenre narrows what EPGStation
// records; rokuban's rule_genres has no equivalent column, so dropping it
// broadens the match set (same class of loss as an ARE-dropped text match)
// and must warn, unlike AllowEndLack which doesn't affect what gets matched.
func TestBuildRuleFields_DroppedSubGenreWarns(t *testing.T) {
	subGenre := int16(1)
	r := epgstation.Rule{
		ID: 7,
		SearchOption: epgstation.RuleSearchOption{
			Genres: []epgstation.RuleGenre{{Genre: 2, SubGenre: &subGenre}},
		},
		ReserveOption: epgstation.RuleReserveOption{Enable: true, AllowEndLack: true},
	}
	fields, warnings := buildRuleFields(r)
	if len(fields.genres) != 1 || fields.genres[0] != 2 {
		t.Fatalf("genres = %v, want [2]", fields.genres)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "subGenre") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one mentioning subGenre", warnings)
	}
}
