package pgstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/store"
)

type compiledRuleQuery struct {
	where            string
	args             []any
	needsServiceJoin bool
}

// compileRuleQuery は条件を epg_programs（エイリアス p）に対する WHERE 句に落とす。
// 空の Sites はフェイルセーフとして rulequery.Validate が拒む（理由はそこに書いた）。
func compileRuleQuery(conditions rulequery.Conditions) (compiledRuleQuery, error) {
	if err := rulequery.Validate(conditions); err != nil {
		return compiledRuleQuery{}, err
	}
	var b strings.Builder
	args := make([]any, 0, 16)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	and := func(clause string) {
		if b.Len() == 0 {
			b.WriteString(clause)
			return
		}
		b.WriteString(" AND ")
		b.WriteString(clause)
	}

	and("p.site = ANY(" + arg(conditions.Sites) + ")")
	if conditions.IsFree != nil {
		and("p.is_free = " + arg(*conditions.IsFree))
	}
	if conditions.DurationMinMs != nil {
		and("p.duration_ms >= " + arg(*conditions.DurationMinMs))
	}
	if conditions.DurationMaxMs != nil {
		and("p.duration_ms <= " + arg(*conditions.DurationMaxMs))
	}
	if conditions.PeriodStartAt != nil {
		and("p.start_at >= " + arg(*conditions.PeriodStartAt))
	}
	if conditions.PeriodEndAt != nil {
		and("p.start_at < " + arg(*conditions.PeriodEndAt))
	}
	if len(conditions.Genres) > 0 {
		// genre_lv1 は smallint[]。重なりがあればマッチ。
		and("p.genre_lv1 && " + arg(conditions.Genres) + "::smallint[]")
	}
	if len(conditions.Services) > 0 {
		parts := make([]string, 0, len(conditions.Services))
		for _, service := range conditions.Services {
			parts = append(parts, fmt.Sprintf("(%s, %s)", arg(service.NetworkID), arg(service.ServiceID)))
		}
		and(fmt.Sprintf("(p.network_id, p.service_id) IN (%s)", strings.Join(parts, ", ")))
	}

	needsServiceJoin := len(conditions.ChannelTypes) > 0
	if needsServiceJoin {
		and("s.channel_type = ANY(" + arg(conditions.ChannelTypes) + ")")
	}
	for _, match := range conditions.TextMatches {
		and(compileTextMatch(match, arg))
	}
	if len(conditions.Times) > 0 {
		parts := make([]string, 0, len(conditions.Times))
		for _, window := range conditions.Times {
			parts = append(parts, "("+compileTimeWindow(window, arg)+")")
		}
		// 複数時間帯は OR（いずれかの窓に入ればよい）
		and("(" + strings.Join(parts, " OR ") + ")")
	}
	// b は常に非空（先頭の p.site 述語が必ず and() を通るため）。
	return compiledRuleQuery{where: b.String(), args: args, needsServiceJoin: needsServiceJoin}, nil
}

func compileTextMatch(match rulequery.TextMatch, arg func(any) string) string {
	column := textTargetColumn(match.Target)
	var clause string
	switch match.Mode {
	case "keyword":
		clause = keywordClause(column, match.Value, match.CaseSensitive, arg)
	case "regex":
		// 正規表現方言は Postgres POSIX ARE（~ / ~*）。ユーザーが書いたパターンの意味を
		// 壊さないよう、列は生のまま使う。
		if match.CaseSensitive {
			clause = column + " ~ " + arg(match.Value)
		} else {
			clause = column + " ~* " + arg(match.Value)
		}
	}
	if match.Negate {
		return "NOT (" + clause + ")"
	}
	return clause
}

// keywordClause は列 col への部分一致を組む。
// 検索と分類ルールで全角・半角の正規化と LIKE 特殊文字の扱いを揃える。
func keywordClause(col string, value string, caseSensitive bool, arg func(any) string) string {
	if caseSensitive {
		return col + " LIKE ('%' || like_escape(" + arg(value) + ") || '%') ESCAPE '\\'"
	}
	normCol := "normalize_search_text(" + col + ")"
	return normCol + " LIKE ('%' || normalize_search_text(like_escape(" + arg(value) + ")) || '%') ESCAPE '\\'"
}

func textTargetColumn(target string) string {
	switch target {
	case "name":
		return "p.name"
	case "description":
		return "p.description"
	case "extended":
		// jsonb をテキスト化して検索する。
		return "coalesce(p.extended::text, '')"
	default:
		return ""
	}
}

// startAtJST は番組開始時刻を JST の壁時計（timestamp）に落とす式。
// 曜日と時刻の両方で使うため 1 箇所で定義する（同じ式を複数箇所に書き下すと
// 連結ミスが混入し、生成 SQL を実行しないテストではすり抜ける）。
const startAtJST = "(p.start_at AT TIME ZONE 'Asia/Tokyo')"

func compileTimeWindow(window rulequery.TimeWindow, arg func(any) string) string {
	// Postgres isodow: 1=月 … 7=日。我々のビットは bit0=月 … bit6=日 なので常に isodow-1。
	dowExpr := "(1 << (EXTRACT(ISODOW FROM " + startAtJST + ")::int - 1))"
	weekdayClause := "(" + dowExpr + " & " + arg(window.Weekdays) + ") <> 0"
	// 時刻は JST の壁時計での「その日の 0 時からの秒」。
	secExpr := "EXTRACT(EPOCH FROM " + startAtJST + "::time)::int"
	var timeClause string
	if window.EndSec >= window.StartSec {
		timeClause = secExpr + " >= " + arg(window.StartSec) + " AND " + secExpr + " < " + arg(window.EndSec)
	} else {
		// 翌日跨ぎ: start..86400 or 0..end
		timeClause = "(" + secExpr + " >= " + arg(window.StartSec) + " OR " + secExpr + " < " + arg(window.EndSec) + ")"
	}
	return weekdayClause + " AND " + timeClause
}

// SearchPrograms は条件にマッチした番組の検索結果行を返す。conditions.Sites が絞り込み対象を
// 指定している場合はそのサイトだけを対象とする。API 検索（internal/api/search.go）が使う。
// site は番組の identity の一部なので、同一放送が複数 site でマッチしても行を畳まない。
func (s *EPG) SearchPrograms(ctx context.Context, conditions rulequery.Conditions) ([]store.ProgramMatch, error) {
	compiled, err := compileRuleQuery(conditions)
	if err != nil {
		return nil, err
	}
	query := `
SELECT p.site, p.program_id, p.network_id, p.service_id,
       p.start_at, p.duration_ms, p.name, p.is_free
FROM epg_programs p
`
	if compiled.needsServiceJoin {
		query += `JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
`
	}
	query += "WHERE " + compiled.where + "\nORDER BY p.program_id, p.site"

	rows, err := s.pool.Query(ctx, query, compiled.args...)
	if err != nil {
		return nil, fmt.Errorf("matching EPG programs: %w", err)
	}
	defer rows.Close()
	var matches []store.ProgramMatch
	for rows.Next() {
		var match store.ProgramMatch
		if err := rows.Scan(
			&match.Site,
			&match.ProgramID,
			&match.NetworkID,
			&match.ServiceID,
			&match.StartAt,
			&match.DurationMs,
			&match.Name,
			&match.IsFree,
		); err != nil {
			return nil, fmt.Errorf("scanning EPG program match: %w", err)
		}
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading EPG program matches: %w", err)
	}
	return matches, nil
}

// MatchProgramIDsForRule は ruleID の条件で、site 1 つ分のマッチする program_id を返す。
// ruler がサイトごとに、rule × site × 定期パスで呼ぶ（1 パスは site のループで
// 全ルールを評価する。docs/recording/ruler.md「サイトの扱い」）。呼び出し頻度が
// 高いホットパスなので、SearchPrograms が返す表示用の 6 列（name は trgm 索引
// 付きの text）は引かず、programID だけの狭い射影で問い合わせる。
//
// rule_sites（conditions.Sites）が非空かつ site を含まなければ、そのルールは site の対象外
// なのでクエリを投げずに空を返す。対象内なら site 1 件に絞って compileRuleQuery の
// WHERE 句だけを共有し、SELECT リストは別に持つ。
func (s *EPG) MatchProgramIDsForRule(ctx context.Context, site string, ruleID int64) ([]int64, error) {
	conditions, err := loadRuleConditions(ctx, sqlcgen.New(s.pool), ruleID)
	if err != nil {
		return nil, err
	}
	if len(conditions.Sites) > 0 && !slices.Contains(conditions.Sites, site) {
		return nil, nil
	}
	conditions.Sites = []string{site}
	compiled, err := compileRuleQuery(conditions)
	if err != nil {
		return nil, err
	}
	query := "SELECT p.program_id FROM epg_programs p\n"
	if compiled.needsServiceJoin {
		query += `JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
`
	}
	query += "WHERE " + compiled.where

	rows, err := s.pool.Query(ctx, query, compiled.args...)
	if err != nil {
		return nil, fmt.Errorf("matching program ids for rule %d: %w", ruleID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning program id for rule %d: %w", ruleID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading program ids for rule %d: %w", ruleID, err)
	}
	return ids, nil
}

// invalidPatternError は Postgres の構文エラーを store.ErrInvalidPattern として返す。
// Error は Postgres の文面だけを返す。API は不正パターンの 400 本文にこの文面をそのまま
// 載せるので、errors.Join の 2 行形式にしない。
type invalidPatternError struct{ err error }

func (e invalidPatternError) Error() string        { return e.err.Error() }
func (e invalidPatternError) Unwrap() error        { return e.err }
func (e invalidPatternError) Is(target error) bool { return target == store.ErrInvalidPattern }

// ValidateRegex は正規表現を独立した Postgres 文で検証する。
func (s *EPG) ValidateRegex(ctx context.Context, pattern string) error {
	if err := sqlcgen.New(s.pool).ValidateRegexPattern(ctx, pattern); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "2201B" {
			return invalidPatternError{err}
		}
		return fmt.Errorf("validating POSIX regular expression: %w", err)
	}
	return nil
}

func loadRuleConditions(ctx context.Context, q *sqlcgen.Queries, ruleID int64) (rulequery.Conditions, error) {
	row, err := q.GetRule(ctx, ruleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return rulequery.Conditions{}, fmt.Errorf("rule %d: %w", ruleID, store.ErrNotFound)
		}
		return rulequery.Conditions{}, fmt.Errorf("getting rule %d: %w", ruleID, err)
	}
	conditions := rulequery.Conditions{
		DurationMinMs: row.DurationMinMs,
		DurationMaxMs: row.DurationMaxMs,
		PeriodStartAt: row.PeriodStartAt,
		PeriodEndAt:   row.PeriodEndAt,
	}
	if row.IsFree.Valid {
		isFree := row.IsFree.Bool
		conditions.IsFree = &isFree
	}

	texts, err := q.ListRuleTextMatches(ctx, ruleID)
	if err != nil {
		return rulequery.Conditions{}, fmt.Errorf("listing text matches for rule %d: %w", ruleID, err)
	}
	for _, match := range texts {
		conditions.TextMatches = append(conditions.TextMatches, rulequery.TextMatch{
			Target:        match.Target,
			Mode:          match.Mode,
			Value:         match.Value,
			CaseSensitive: match.CaseSensitive,
			Negate:        match.Negate,
		})
	}

	services, err := q.ListRuleServices(ctx, ruleID)
	if err != nil {
		return rulequery.Conditions{}, fmt.Errorf("listing services for rule %d: %w", ruleID, err)
	}
	for _, service := range services {
		conditions.Services = append(conditions.Services, rulequery.ServiceRef{NetworkID: service.NetworkID, ServiceID: service.ServiceID})
	}

	channelTypes, err := q.ListRuleChannelTypes(ctx, ruleID)
	if err != nil {
		return rulequery.Conditions{}, fmt.Errorf("listing channel types for rule %d: %w", ruleID, err)
	}
	for _, channelType := range channelTypes {
		conditions.ChannelTypes = append(conditions.ChannelTypes, channelType.ChannelType)
	}

	genres, err := q.ListRuleGenres(ctx, ruleID)
	if err != nil {
		return rulequery.Conditions{}, fmt.Errorf("listing genres for rule %d: %w", ruleID, err)
	}
	for _, genre := range genres {
		conditions.Genres = append(conditions.Genres, genre.GenreLv1)
	}

	times, err := q.ListRuleTimes(ctx, ruleID)
	if err != nil {
		return rulequery.Conditions{}, fmt.Errorf("listing times for rule %d: %w", ruleID, err)
	}
	for _, window := range times {
		conditions.Times = append(conditions.Times, rulequery.TimeWindow{
			Weekdays: int(window.Weekdays),
			StartSec: int(window.StartSec),
			EndSec:   int(window.EndSec),
		})
	}

	sites, err := q.ListRuleSites(ctx, ruleID)
	if err != nil {
		return rulequery.Conditions{}, fmt.Errorf("listing sites for rule %d: %w", ruleID, err)
	}
	for _, site := range sites {
		conditions.Sites = append(conditions.Sites, site.Site)
	}
	return conditions, nil
}
