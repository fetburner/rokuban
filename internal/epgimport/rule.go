package epgimport

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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
// 冪等キーは rules.metadata の jsonb containment
// {"epgstation":{"ruleId": <EPGStation の rule id>}}。再実行では
// FindRuleByMetadata で既存行を引き、子テーブルは一度消してから入れ直す
// （internal/catalog/rescue.go の upsertRule と同じ流儀）ので行は増えない。
//
// 1 ルールにつき 1 トランザクション（pool.Begin）で「ルール本体 + 子
// テーブル全部」をまとめてコミットする。子テーブルの 1 つの INSERT だけが
// 失敗する状況（例: rule_sites の CHECK 違反）で、ルール本体と他の子
// テーブルだけ書き込まれた「一部だけ narrow なルール」が残る事故を防ぐ
// （途中失敗すれば丸ごと消える。TestImportRules_ChildInsertFailureRollsBack
// が実測で固定している）。
//
// site は rule_sites に 1 行だけ入れる対象サイト（EPGStation は単一の
// mirakc/チューナー源を前提にした単一サイト運用なので、全サイトではなく
// このサイトへスコープする）。
func ImportRules(ctx context.Context, pool *pgxpool.Pool, site string, rules []epgstation.Rule) (RuleImportResult, error) {
	var res RuleImportResult
	for _, r := range rules {
		if r.IsTimeSpecification {
			res.Skipped++
			res.Warnings = append(res.Warnings, RuleWarning{r.ID,
				"time-specified rule (programId を持たない予約) は rokuban が機能ごと落としている対象のためスキップした"})
			continue
		}

		created, warnings, err := importOneRule(ctx, pool, site, r)
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

// importOneRule は 1 ルール分の変換 + DB 書き込みを行う。ARE 検証は
// トランザクション開始前に pool へ直接投げ（読み取り専用の判定であり、かつ
// Postgres 側のエラーで tx を abort させないため）、ルール本体 + 子テーブル
// の書き込みだけを 1 トランザクションにまとめる。
func importOneRule(ctx context.Context, pool *pgxpool.Pool, site string, r epgstation.Rule) (created bool, warnings []string, err error) {
	fields, warnings := buildRuleFields(r)

	// ARE 非互換の正規表現は「警告して、その text match だけ落とす」。
	// そのまま insert すると、以降このルールの評価（rulequery が組む
	// `col ~ pattern` 節）が毎パス失敗し続ける恒久的な壊れ方になるため、
	// 「警告に出す」（受け入れ基準）は満たしつつ実害（評価不能なルール）は
	// 避ける。
	//
	// 検証は pool 直付けの Queries（トランザクション開始前）で行う。
	// 不正な正規表現は Postgres 側で SQL エラーになる制御フローの一部
	// （珍しくもバグでもない）なので、下で開くトランザクションの中で
	// 呼ぶとそのままトランザクションを abort 状態にしてしまい、以降の
	// FindRuleByMetadata 等がすべて「current transaction is aborted」で
	// 落ちる（実測して見つけた）。読み取り専用の検証なのでトランザクション
	// に含める理由もない。
	validateQ := sqlcgen.New(pool)
	kept := fields.textMatches[:0]
	for _, m := range fields.textMatches {
		if m.Mode != "regex" {
			kept = append(kept, m)
			continue
		}
		if err := validateQ.ValidateRegexPattern(ctx, m.Value); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"regex %q is not POSIX ARE compatible (%v) — text match skipped, fix and add it back manually", m.Value, err))
			continue
		}
		kept = append(kept, m)
	}
	fields.textMatches = kept

	// 変換の結果、絞り込み条件が 1 つも残らなかったルールを enabled のまま
	// 作らない。rulequery.Compile は条件が空なら "TRUE"（サイト一致のみ）に
	// 縮退するため、有効なままだと EPG 全番組を録画する巨大ルールになる
	// （ARE 非互換で唯一の text match が落ちた場合や、EPGStation 側の
	// ルールがもともと無条件だった場合の両方で起こりうる）。
	if !fields.hasNarrowingCondition() {
		fields.enabled = false
		warnings = append(warnings,
			"rule has no narrowing conditions after conversion (would match every program in the EPG) — imported disabled; review conditions and enable manually")
	}

	metadata, err := json.Marshal(map[string]any{"epgstation": map[string]any{"ruleId": r.ID}})
	if err != nil {
		return false, warnings, fmt.Errorf("marshalling metadata for epgstation rule %d: %w", r.ID, err)
	}

	// ルール本体 + 子テーブル全部を 1 トランザクションにまとめる。途中の
	// INSERT が 1 つでも失敗すれば丸ごとロールバックされ、「一部の条件だけ
	// 反映されたルール」を残さない（TestImportRules_ChildInsertFailureRollsBack）。
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, warnings, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	existing, err := q.FindRuleByMetadata(ctx, metadata)
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
			return false, warnings, fmt.Errorf("updating rule: %w", err)
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
			return false, warnings, fmt.Errorf("creating rule: %w", err)
		}
		ruleID = row.ID
	default:
		return false, warnings, fmt.Errorf("looking up rule: %w", err)
	}

	if err := writeRuleChildren(ctx, q, ruleID, site, fields); err != nil {
		return false, warnings, fmt.Errorf("writing children: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, warnings, fmt.Errorf("committing: %w", err)
	}
	return created, warnings, nil
}

// writeRuleChildren は子テーブルを「一度消してから入れ直す」で冪等に反映する
// （internal/catalog/rescue.go の upsertRule と同じ流儀）。呼び出し側
// （importOneRule）がトランザクションに包む。
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
	// site は EPGStation に無い次元なので必ず 1 件（対象サイトそのもの）。
	// CHECK (site <> '') に違反すると呼び出し側のトランザクションが
	// ロールバックされ、ここまでの INSERT も含めて丸ごと消える
	// （TestImportRules_ChildInsertFailureRollsBack）。
	if err := q.InsertRuleSite(ctx, sqlcgen.InsertRuleSiteParams{RuleID: ruleID, Site: site}); err != nil {
		return fmt.Errorf("inserting site: %w", err)
	}
	return nil
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
