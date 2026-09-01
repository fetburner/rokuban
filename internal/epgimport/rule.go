package epgimport

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// RuleWarning は 1 ルールの変換で生じた警告 1 件。
type RuleWarning struct {
	EpgstationRuleID int64
	Message          string
}

// RuleImportResult は ImportRules の結果。
type RuleImportResult struct {
	Created  int
	Updated  int
	Skipped  int
	Warnings []RuleWarning
}

// ruleFields は 1 ルール分の変換結果（rules 本体 + 子テーブル）。
type ruleFields struct {
	name             string
	enabled          bool
	isFree           *bool
	durationMinMs    *int64
	durationMaxMs    *int64
	periodStartAt    *int64 // UnixtimeMS
	periodEndAt      *int64
	filenameTemplate string
	textMatches      []sqlcgen.InsertRuleTextMatchParams
	services         []sqlcgen.InsertRuleServiceParams
	channelTypes     []string
	genres           []int16
	times            []sqlcgen.InsertRuleTimeParams
}

// ImportRules は EPGStation のルールを rokuban の rules（+ 子テーブル）へ
// 冪等に取り込む。
//
// 冪等キーは rules.metadata の jsonb containment
// {"epgstation":{"ruleId": <EPGStation の rule id>}}。再実行では
// FindRuleByMetadata で既存行を引き、子テーブルは一度消してから入れ直す
// （internal/catalog/rescue.go の upsertRule と同じ流儀）ので行は増えない。
//
// site は rule_sites に 1 行だけ入れる対象サイト（EPGStation は単一の
// mirakc/チューナー源を前提にした単一サイト運用なので、全サイトではなく
// このサイトへスコープする）。
func ImportRules(ctx context.Context, q *sqlcgen.Queries, site string, rules []epgstation.Rule) (RuleImportResult, error) {
	var res RuleImportResult
	for _, r := range rules {
		if r.IsTimeSpecification {
			res.Skipped++
			res.Warnings = append(res.Warnings, RuleWarning{r.ID,
				"time-specified rule (programId を持たない予約) は rokuban が機能ごと落としている対象のためスキップした"})
			continue
		}

		fields, warnings := buildRuleFields(r)
		for _, w := range warnings {
			res.Warnings = append(res.Warnings, RuleWarning{r.ID, w})
		}

		// ARE 非互換の正規表現は「警告して、その text match だけ落とす」。
		// そのまま insert すると、以降このルールの評価（rulequery が組む
		// `col ~ pattern` 節）が毎パス失敗し続ける恒久的な壊れ方になるため、
		// 「警告に出す」（受け入れ基準）は満たしつつ実害（評価不能なルール）は
		// 避ける。
		kept := fields.textMatches[:0]
		for _, m := range fields.textMatches {
			if m.Mode != "regex" {
				kept = append(kept, m)
				continue
			}
			if err := q.ValidateRegexPattern(ctx, m.Value); err != nil {
				res.Warnings = append(res.Warnings, RuleWarning{r.ID,
					fmt.Sprintf("regex %q is not POSIX ARE compatible (%v) — text match skipped, fix and add it back manually", m.Value, err)})
				continue
			}
			kept = append(kept, m)
		}
		fields.textMatches = kept

		metadata, err := json.Marshal(map[string]any{"epgstation": map[string]any{"ruleId": r.ID}})
		if err != nil {
			return res, fmt.Errorf("marshalling metadata for epgstation rule %d: %w", r.ID, err)
		}

		existing, err := q.FindRuleByMetadata(ctx, metadata)
		created := false
		var ruleID int64
		switch {
		case err == nil:
			ruleID = existing.ID
			if _, err := q.UpdateRule(ctx, sqlcgen.UpdateRuleParams{
				ID:               ruleID,
				Name:             fields.name,
				Description:      existing.Description,
				Enabled:          fields.enabled,
				Priority:         existing.Priority,
				IsFree:           boolToPg(fields.isFree),
				DurationMinMs:    fields.durationMinMs,
				DurationMaxMs:    fields.durationMaxMs,
				PeriodStartAt:    msToTimePtr(fields.periodStartAt),
				PeriodEndAt:      msToTimePtr(fields.periodEndAt),
				DedupeEnabled:    existing.DedupeEnabled,
				DedupeThreshold:  existing.DedupeThreshold,
				DedupeWindow:     existing.DedupeWindow,
				KeepOriginal:     existing.KeepOriginal,
				EncodeProfiles:   existing.EncodeProfiles,
				FilenameTemplate: fields.filenameTemplate,
				Metadata:         metadata,
			}); err != nil {
				return res, fmt.Errorf("updating rule for epgstation rule %d: %w", r.ID, err)
			}
		case isNoRows(err):
			created = true
			row, err := q.CreateRule(ctx, sqlcgen.CreateRuleParams{
				Name:             fields.name,
				Description:      "",
				Enabled:          fields.enabled,
				Priority:         10,
				IsFree:           boolToPg(fields.isFree),
				DurationMinMs:    fields.durationMinMs,
				DurationMaxMs:    fields.durationMaxMs,
				PeriodStartAt:    msToTimePtr(fields.periodStartAt),
				PeriodEndAt:      msToTimePtr(fields.periodEndAt),
				DedupeEnabled:    false,
				KeepOriginal:     "always",
				EncodeProfiles:   []string{},
				FilenameTemplate: fields.filenameTemplate,
				Metadata:         metadata,
			})
			if err != nil {
				return res, fmt.Errorf("creating rule for epgstation rule %d: %w", r.ID, err)
			}
			ruleID = row.ID
		default:
			return res, fmt.Errorf("looking up rule for epgstation rule %d: %w", r.ID, err)
		}

		if err := writeRuleChildren(ctx, q, ruleID, site, fields); err != nil {
			return res, fmt.Errorf("writing children for epgstation rule %d: %w", r.ID, err)
		}

		if created {
			res.Created++
		} else {
			res.Updated++
		}
	}
	return res, nil
}

// writeRuleChildren は子テーブルを「一度消してから入れ直す」で冪等に反映する
// （internal/catalog/rescue.go の upsertRule と同じ流儀）。
func writeRuleChildren(ctx context.Context, q *sqlcgen.Queries, ruleID int64, site string, f ruleFields) error {
	if err := q.DeleteRuleTextMatches(ctx, ruleID); err != nil {
		return fmt.Errorf("deleting text matches: %w", err)
	}
	if err := q.DeleteRuleServices(ctx, ruleID); err != nil {
		return fmt.Errorf("deleting services: %w", err)
	}
	if err := q.DeleteRuleChannelTypes(ctx, ruleID); err != nil {
		return fmt.Errorf("deleting channel types: %w", err)
	}
	if err := q.DeleteRuleGenres(ctx, ruleID); err != nil {
		return fmt.Errorf("deleting genres: %w", err)
	}
	if err := q.DeleteRuleTimes(ctx, ruleID); err != nil {
		return fmt.Errorf("deleting times: %w", err)
	}
	if err := q.DeleteRuleSites(ctx, ruleID); err != nil {
		return fmt.Errorf("deleting sites: %w", err)
	}

	for i, m := range f.textMatches {
		m.RuleID = ruleID
		m.Seq = int32(i)
		if err := q.InsertRuleTextMatch(ctx, m); err != nil {
			return fmt.Errorf("inserting text match seq=%d: %w", i, err)
		}
	}
	for _, s := range f.services {
		s.RuleID = ruleID
		if err := q.InsertRuleService(ctx, s); err != nil {
			return fmt.Errorf("inserting service: %w", err)
		}
	}
	for _, ct := range f.channelTypes {
		if err := q.InsertRuleChannelType(ctx, sqlcgen.InsertRuleChannelTypeParams{RuleID: ruleID, ChannelType: ct}); err != nil {
			return fmt.Errorf("inserting channel type %s: %w", ct, err)
		}
	}
	for _, g := range f.genres {
		if err := q.InsertRuleGenre(ctx, sqlcgen.InsertRuleGenreParams{RuleID: ruleID, GenreLv1: g}); err != nil {
			return fmt.Errorf("inserting genre %d: %w", g, err)
		}
	}
	for i, t := range f.times {
		t.RuleID = ruleID
		t.Seq = int32(i)
		if err := q.InsertRuleTime(ctx, t); err != nil {
			return fmt.Errorf("inserting time seq=%d: %w", i, err)
		}
	}
	if err := q.InsertRuleSite(ctx, sqlcgen.InsertRuleSiteParams{RuleID: ruleID, Site: site}); err != nil {
		return fmt.Errorf("inserting site: %w", err)
	}
	return nil
}

// buildRuleFields は 1 つの EPGStation ルールを rokuban の形へ変換する。
// DB へは触れない純関数（ARE 検証・DB 往復は ImportRules 側の責務）。
func buildRuleFields(r epgstation.Rule) (ruleFields, []string) {
	var warnings []string
	f := ruleFields{enabled: r.ReserveOption.Enable}

	// name: rokuban の rules.name は NOT NULL だが EPGStation のルールに
	// 名前フィールドは無い（api.d.ts の AddRuleOption 参照）。キーワードを
	// 代用し、キーワードも無ければ id で識別可能なプレースホルダにする。
	switch {
	case r.SearchOption.Keyword != "":
		f.name = r.SearchOption.Keyword
	default:
		f.name = fmt.Sprintf("EPGStation rule %d", r.ID)
	}

	f.isFree = r.SearchOption.IsFree

	// durationMin/Max の単位は未検証（epgstation.RuleSearchOption のコメント
	// 参照）。SearchTime.range と同じ「秒」という前提で ms に変換する。
	if r.SearchOption.DurationMin != nil {
		v := *r.SearchOption.DurationMin * 1000
		f.durationMinMs = &v
	}
	if r.SearchOption.DurationMax != nil {
		v := *r.SearchOption.DurationMax * 1000
		f.durationMaxMs = &v
	}

	if len(r.SearchOption.SearchPeriods) > 0 {
		p := r.SearchOption.SearchPeriods[0]
		f.periodStartAt = &p.StartAt
		f.periodEndAt = &p.EndAt
		if len(r.SearchOption.SearchPeriods) > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"rule has %d searchPeriods but rokuban rules only support one period_start_at/period_end_at — only the first was kept",
				len(r.SearchOption.SearchPeriods)))
		}
	}

	f.textMatches = buildTextMatches(r.SearchOption, &warnings)

	for _, g := range r.SearchOption.Genres {
		f.genres = append(f.genres, g.Genre)
	}

	if r.SearchOption.GR {
		f.channelTypes = append(f.channelTypes, "GR")
	}
	if r.SearchOption.BS {
		f.channelTypes = append(f.channelTypes, "BS")
	}
	if r.SearchOption.CS {
		f.channelTypes = append(f.channelTypes, "CS")
	}
	if r.SearchOption.SKY {
		f.channelTypes = append(f.channelTypes, "SKY")
	}

	for _, id := range r.SearchOption.ChannelIDs {
		networkID, serviceID := mirakc.SplitServiceID(id)
		f.services = append(f.services, sqlcgen.InsertRuleServiceParams{
			NetworkID: int32(networkID),
			ServiceID: int32(serviceID),
		})
	}

	for _, t := range r.SearchOption.Times {
		weekdays := epgWeekdayToRokuban(t.Week)
		if weekdays == 0 {
			warnings = append(warnings, fmt.Sprintf("time window with week=%d selects no weekday — skipped", t.Week))
			continue
		}
		if t.Start == nil || t.Range == nil {
			warnings = append(warnings, "time window is missing start or range — skipped")
			continue
		}
		startSec := (*t.Start % 24) * 3600
		endHour := *t.Start + *t.Range
		endSec := (endHour % 24) * 3600
		f.times = append(f.times, sqlcgen.InsertRuleTimeParams{
			Weekdays: int32(weekdays),
			StartSec: int32(startSec),
			EndSec:   int32(endSec),
		})
	}

	if r.SearchOption.DurationMin != nil || r.SearchOption.DurationMax != nil {
		warnings = append(warnings, "durationMin/durationMax were converted assuming seconds (EPGStation source does not document the unit) — verify against the original rule")
	}

	if r.ReserveOption.AvoidDuplicate {
		warnings = append(warnings, "avoidDuplicate was enabled in EPGStation but rokuban's dedupe needs a similarity threshold EPGStation doesn't have — imported with dedupe disabled; enable rules.dedupe_enabled/dedupe_threshold manually if wanted")
	}

	if r.SaveOption != nil && r.SaveOption.RecordedFormat != "" {
		tmpl, unsupported := ConvertRecordedFormat(r.SaveOption.RecordedFormat)
		if len(unsupported) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"recordedFormat %q uses unsupported variable(s) %v — filenameTemplate left unset (rokuban's DefaultTemplate applies instead)",
				r.SaveOption.RecordedFormat, unsupported))
		} else if err := contentpath.Validate(tmpl); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"recordedFormat %q converted to %q but failed rokuban template validation (%v) — filenameTemplate left unset",
				r.SaveOption.RecordedFormat, tmpl, err))
		} else {
			f.filenameTemplate = tmpl
		}
	}

	return f, warnings
}

// buildTextMatches は keyword/ignoreKeyword を rule_text_matches の行群へ
// 変換する。
//
// EPGStation は「1 つのキーワードを name/description/extended のうち選んだ
// 複数フィールドに OR で照合する」という形だが、rokuban の rule_text_matches
// は行を単純 AND で結合する（internal/rulequery/compile.go）ので OR は
// 表現できない。
//
//   - ignoreKeyword（除外）は De Morgan の法則で救える: 「いずれかの
//     フィールドにヒットしたら除外」は「どのフィールドにもヒットしない」の
//     否定と同じなので、対象フィールドごとに negate=true の行を出し
//     AND で結んでも意味は変わらない。
//   - 肯定の keyword は救えない。複数フィールドを選んでいたら警告し、
//     name > description > extended の優先順で 1 フィールドだけ残す
//     （選ばれなかったフィールドはこのルールの絞り込みから消える —— 一致
//     条件が緩む側に倒れるので録り逃しにはならないが、意図と異なりうる）。
func buildTextMatches(opt epgstation.RuleSearchOption, warnings *[]string) []sqlcgen.InsertRuleTextMatchParams {
	var out []sqlcgen.InsertRuleTextMatchParams

	if opt.Keyword != "" {
		targets := selectedTargets(opt.Name, opt.Description, opt.Extended)
		if len(targets) == 0 {
			targets = []string{"name"}
		}
		if len(targets) > 1 {
			*warnings = append(*warnings, fmt.Sprintf(
				"keyword %q matches multiple fields (%v) in EPGStation (OR semantics); rokuban rule_text_matches only supports AND, so only %q was kept",
				opt.Keyword, targets, targets[0]))
			targets = targets[:1]
		}
		mode := "keyword"
		if opt.KeyRegExp {
			mode = "regex"
		}
		out = append(out, sqlcgen.InsertRuleTextMatchParams{
			Target: targets[0], Mode: mode, Value: opt.Keyword, CaseSensitive: opt.KeyCS,
		})
	}

	if opt.IgnoreKeyword != "" {
		targets := selectedTargets(opt.IgnoreName, opt.IgnoreDescription, opt.IgnoreExtended)
		if len(targets) == 0 {
			targets = []string{"name"}
		}
		mode := "keyword"
		if opt.IgnoreKeyRegExp {
			mode = "regex"
		}
		for _, target := range targets {
			out = append(out, sqlcgen.InsertRuleTextMatchParams{
				Target: target, Mode: mode, Value: opt.IgnoreKeyword, CaseSensitive: opt.IgnoreKeyCS, Negate: true,
			})
		}
	}

	return out
}

func selectedTargets(name, description, extended bool) []string {
	var out []string
	if name {
		out = append(out, "name")
	}
	if description {
		out = append(out, "description")
	}
	if extended {
		out = append(out, "extended")
	}
	return out
}

// epgWeekdayToRokuban は EPGStation の曜日ビット（bit0=日 … bit6=土。
// internal/epgstation.RuleSearchTime のコメント参照）を rokuban の
// rule_times.weekdays（bit0=月 … bit6=日。docs/schema/rules.md）へ並べ替える。
func epgWeekdayToRokuban(epg int) int {
	var out int
	for i := 0; i < 7; i++ {
		if epg&(1<<i) == 0 {
			continue
		}
		rb := i - 1
		if i == 0 {
			rb = 6 // 日: epg bit0 → rokuban bit6
		}
		out |= 1 << rb
	}
	return out
}

func boolToPg(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}
