package epgimport

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/programid"
	"github.com/fetburner/rokuban/internal/store"
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
	textMatches      []store.RuleTextMatch
	services         []store.RuleService
	channelTypes     []string
	genres           []int16
	times            []store.RuleTimeWindow
}

// hasNarrowingCondition は、このルールが EPG を絞り込む条件を 1 つでも
// 持っているかを返す。rule_sites は数えない —— EPGStation 自体には無い
// 次元で、import が付け足す「対象サイトのスコープ」であり、ユーザーが
// 選んだ絞り込みではないため。
func (f ruleFields) hasNarrowingCondition() bool {
	return len(f.textMatches) > 0 ||
		len(f.services) > 0 ||
		len(f.channelTypes) > 0 ||
		len(f.genres) > 0 ||
		len(f.times) > 0 ||
		f.isFree != nil ||
		f.durationMinMs != nil ||
		f.durationMaxMs != nil ||
		f.periodStartAt != nil ||
		f.periodEndAt != nil
}

// ImportRules は EPGStation のルールを rokuban の rules（+ 子テーブル）へ
// 冪等に取り込む。
//
// store.RuleImport が EPGStation rule ID で既存行を引き、作成または更新するため、
// 再実行しても行は増えない。ルール本体と子条件は store が 1 つの tx で確定し、
// 子条件の挿入が失敗した場合は本体も残らない（TestRulesContract/EPGStation import uses stable identity atomically）。
//
// site は rule_sites に 1 行だけ入れる対象サイト（EPGStation は単一の
// mirakc/チューナー源を前提にした単一サイト運用なので、全サイトではなく
// このサイトへスコープする）。
func ImportRules(ctx context.Context, ruleStore store.RuleImport, programSearch store.ProgramSearch, site string, rules []epgstation.Rule) (RuleImportResult, error) {
	var res RuleImportResult
	for _, r := range rules {
		if r.IsTimeSpecification {
			res.Skipped++
			res.Warnings = append(res.Warnings, RuleWarning{r.ID,
				"time-specified rule (programId を持たない予約) は rokuban が機能ごと落としている対象のためスキップした"})
			continue
		}

		created, warnings, err := importOneRule(ctx, ruleStore, programSearch, site, r)
		// warnings が集まった後にエラーで落ちることもある（例: ARE 警告を
		// 出した直後に子テーブルの INSERT が失敗）。エラーだからといって
		// 警告を捨てると、操作者はエラーメッセージしか見えず、同じ warning
		// が出ていた事実を失う。
		for _, w := range warnings {
			res.Warnings = append(res.Warnings, RuleWarning{r.ID, w})
		}
		if err != nil {
			return res, fmt.Errorf("importing epgstation rule %d: %w", r.ID, err)
		}
		if created {
			res.Created++
		} else {
			res.Updated++
		}
	}
	return res, nil
}

// importOneRule は 1 ルール分を変換し、regex を検証してから store へ渡す。
func importOneRule(ctx context.Context, ruleStore store.RuleImport, programSearch store.ProgramSearch, site string, r epgstation.Rule) (created bool, warnings []string, err error) {
	fields, warnings := buildRuleFields(r)

	// ARE 非互換の正規表現は「警告して、その text match だけ落とす」。
	// そのまま insert すると、以降このルールの評価（pgstore が組む
	// `col ~ pattern` 節）が毎パス失敗し続ける恒久的な壊れ方になるため、
	// 「警告に出す」（受け入れ基準）は満たしつつ実害（評価不能なルール）は
	// 避ける。
	//
	kept := fields.textMatches[:0]
	for _, m := range fields.textMatches {
		if m.Mode != "regex" {
			kept = append(kept, m)
			continue
		}
		if err := programSearch.ValidateRegex(ctx, m.Value); err != nil {
			if !errors.Is(err, store.ErrInvalidPattern) {
				return false, warnings, fmt.Errorf("validating regex %q: %w", m.Value, err)
			}
			warnings = append(warnings, fmt.Sprintf(
				"regex %q is not POSIX ARE compatible (%v) — text match skipped, fix and add it back manually", m.Value, err))
			continue
		}
		kept = append(kept, m)
	}
	fields.textMatches = kept

	// 変換の結果、絞り込み条件が 1 つも残らなかったルールを enabled のまま
	// 作らない。条件が空ならサイト一致のみに縮退するため、
	// 有効なままだと EPG 全番組を録画する巨大ルールになる
	// （ARE 非互換で唯一の text match が落ちた場合や、EPGStation 側の
	// ルールがもともと無条件だった場合の両方で起こりうる）。
	if !fields.hasNarrowingCondition() {
		fields.enabled = false
		warnings = append(warnings,
			"rule has no narrowing conditions after conversion (would match every program in the EPG) — imported disabled; review conditions and enable manually")
	}

	enabled := fields.enabled
	filenameTemplate := fields.filenameTemplate
	in := store.RuleInput{
		Name: fields.name, Enabled: &enabled, IsFree: fields.isFree,
		DurationMinMs: fields.durationMinMs, DurationMaxMs: fields.durationMaxMs,
		PeriodStartAt: msToTimePtr(fields.periodStartAt), PeriodEndAt: msToTimePtr(fields.periodEndAt),
		FilenameTemplate: &filenameTemplate,
		TextMatches:      fields.textMatches, Services: fields.services, ChannelTypes: fields.channelTypes,
		Genres: fields.genres, Times: fields.times, Sites: []string{site},
	}
	created, err = ruleStore.ImportEPGStationRule(ctx, r.ID, in)
	if err != nil {
		return false, warnings, err
	}
	return created, warnings, nil
}

// buildRuleFields は 1 つの EPGStation ルールを rokuban の形へ変換する。
// DB へは触れない純関数（ARE 検証・DB 往復は importOneRule 側の責務）。
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

	// isFree は EPGStation では tristate ではない: DB 列は
	// `"isFree" boolean NOT NULL DEFAULT (0)` で、RuleDB.ts が
	// `!!rule.searchOption.isFree` を無条件に書き戻すため、GET /api/rules
	// はチェックしていないルールにも常に `"isFree": false` を返す（省略され
	// ない。ProgramDB.ts の setFreeQuery も `isFree === false` を「絞り込み
	// なし」として扱い、true のときだけ WHERE 句を足す）。ここで *bool を
	// そのまま通すと、無料放送を絞っていないほとんどのルールが「有料放送
	// だけ」を意味する `is_free = false` に化けて実質何も録れなくなる
	// （false を「絞り込みなし」と区別できないのが原因）。
	if r.SearchOption.IsFree != nil && *r.SearchOption.IsFree {
		f.isFree = r.SearchOption.IsFree
	}

	// durationMin/Max は秒単位（EPGStation の src/model/db/ProgramDB.ts の
	// setDurationMinQuery/setDurationMaxQuery が `option.durationMin * 1000`
	// を ms 単位の duration 列と比較しているので確認済み。SearchTime.range
	// も同じ単位系）。
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

	// SubGenre（ジャンルの下位区分）は rokuban の rule_genres が持たない
	// （genre_lv1 まで）。EncodeProfiles/AvoidDuplicate 同様「絞り込みが
	// 緩む側に倒れる」損失なので警告する —— AllowEndLack のような録画後
	// 挙動の設定（マッチする番組の集合を変えない）とは違うクラス。
	droppedSubGenre := false
	for _, g := range r.SearchOption.Genres {
		f.genres = append(f.genres, g.Genre)
		if g.SubGenre != nil {
			droppedSubGenre = true
		}
	}
	if droppedSubGenre {
		warnings = append(warnings,
			"genre subGenre (finer-grained genre filter) has no rokuban equivalent (rule_genres only has the top-level genre) — dropped, which broadens what matches; review manually")
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
		networkID, serviceID := programid.SplitServiceID(id)
		f.services = append(f.services, store.RuleService{
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
		f.times = append(f.times, store.RuleTimeWindow{
			Weekdays: int32(weekdays),
			StartSec: int32(startSec),
			EndSec:   int32(endSec),
		})
	}

	// AllowEndLack（末尾切れを許可するか）は録画完了の許容度であって EPG の
	// 絞り込み条件ではない（マッチする番組の集合を変えない）。rokuban の
	// rules に対応する列が無く、無くても「録れる番組の集合」は変わらない
	// ので、他の絞り込み条件の損失（droppedSubGenre 等）と違って警告しない。

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
// は行を単純 AND で結合する（internal/store/pgstore/rulesql.go）ので OR は
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
func buildTextMatches(opt epgstation.RuleSearchOption, warnings *[]string) []store.RuleTextMatch {
	var out []store.RuleTextMatch

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
		out = append(out, store.RuleTextMatch{
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
			out = append(out, store.RuleTextMatch{
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
