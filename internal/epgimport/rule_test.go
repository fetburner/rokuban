package epgimport

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
	q := sqlcgen.New(pool)
	ctx := context.Background()

	rules := []epgstation.Rule{basicRule(1, "ニュース")}

	first, err := ImportRules(ctx, q, "default", rules)
	if err != nil {
		t.Fatalf("first ImportRules: %v", err)
	}
	if first.Created != 1 || first.Updated != 0 {
		t.Fatalf("first result = %+v, want Created=1 Updated=0", first)
	}

	second, err := ImportRules(ctx, q, "default", rules)
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
// is not inserted (so the rule stays evaluable).
//
// Pattern choice note (measured, not assumed): docs/data/search.md claims
// Postgres ARE supports lookahead but not lookbehind ("先読み `(?=)` 可・
// 後読み不可"). Measured directly against this task's Postgres 16.2: an
// empty-string match against pattern (?<=foo)bar does NOT error, and
// matching "xyzabc" and "zzzabc" against (?<=xyz)abc return true/false
// respectively — i.e. lookbehind actually works here, contradicting that
// doc sentence. A pattern this Postgres genuinely rejects (measured) is
// JS-style named capture groups, (?<name>...): matching an empty string
// against that pattern raises "quantifier operand invalid". Flagged in the
// PR report; not fixed here (out of this task's scope and only measured
// against one PG version).
func TestImportRules_AREIncompatibleRegexWarns(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
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

	result, err := ImportRules(ctx, q, "default", rules)
	if err != nil {
		t.Fatalf("ImportRules: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("want a warning for ARE-incompatible regex, got none")
	}
	found := false
	for _, w := range result.Warnings {
		if w.EpgstationRuleID == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %+v, want one for epgstation rule 2", result.Warnings)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rule_text_matches`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rule_text_matches count = %d, want 0 (incompatible regex must not be persisted)", count)
	}
}

// TestImportRules_UnsupportedTemplateVariableWarns is the acceptance
// criterion: an unsupported %変数% (e.g. %CHNAME%) must not silently become
// an empty filenameTemplate producing garbled paths — it must warn and fall
// back to the rokuban default template (empty filename_template column).
func TestImportRules_UnsupportedTemplateVariableWarns(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID:            3,
		SearchOption:  epgstation.RuleSearchOption{Keyword: "foo", Name: true},
		ReserveOption: epgstation.RuleReserveOption{Enable: true, AllowEndLack: true},
		SaveOption:    &epgstation.ReserveSaveOption{RecordedFormat: "%CHNAME%_%TITLE%"},
	}}

	result, err := ImportRules(ctx, q, "default", rules)
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
	q := sqlcgen.New(pool)
	ctx := context.Background()

	rules := []epgstation.Rule{{
		ID:                  4,
		IsTimeSpecification: true,
		ReserveOption:       epgstation.RuleReserveOption{Enable: true},
	}}

	result, err := ImportRules(ctx, q, "default", rules)
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
