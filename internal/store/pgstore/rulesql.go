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
	"github.com/fetburner/rokuban/internal/searchsql"
	"github.com/fetburner/rokuban/internal/store"
)

type compiledRuleQuery struct {
	where            string
	args             []any
	needsServiceJoin bool
}

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
		and("(" + strings.Join(parts, " OR ") + ")")
	}
	return compiledRuleQuery{where: b.String(), args: args, needsServiceJoin: needsServiceJoin}, nil
}

func compileTextMatch(match rulequery.TextMatch, arg func(any) string) string {
	column := textTargetColumn(match.Target)
	var clause string
	switch match.Mode {
	case "keyword":
		clause = KeywordClause(column, match.Value, match.CaseSensitive, arg)
	case "regex":
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

// KeywordClause は共有の PostgreSQL キーワード検索述語を組み立てる。
func KeywordClause(column, value string, caseSensitive bool, arg func(any) string) string {
	return searchsql.KeywordClause(column, value, caseSensitive, arg)
}

func textTargetColumn(target string) string {
	switch target {
	case "name":
		return "p.name"
	case "description":
		return "p.description"
	case "extended":
		return "coalesce(p.extended::text, '')"
	default:
		return ""
	}
}

const startAtJST = "(p.start_at AT TIME ZONE 'Asia/Tokyo')"

func compileTimeWindow(window rulequery.TimeWindow, arg func(any) string) string {
	dowExpr := "(1 << (EXTRACT(ISODOW FROM " + startAtJST + ")::int - 1))"
	weekdayClause := "(" + dowExpr + " & " + arg(window.Weekdays) + ") <> 0"
	secExpr := "EXTRACT(EPOCH FROM " + startAtJST + "::time)::int"
	var timeClause string
	if window.EndSec >= window.StartSec {
		timeClause = secExpr + " >= " + arg(window.StartSec) + " AND " + secExpr + " < " + arg(window.EndSec)
	} else {
		timeClause = "(" + secExpr + " >= " + arg(window.StartSec) + " OR " + secExpr + " < " + arg(window.EndSec) + ")"
	}
	return weekdayClause + " AND " + timeClause
}

// SearchPrograms は条件に一致する番組を表示用の射影で返す。
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

// MatchProgramIDsForRule は ruleID のすべての条件を 1 site の番組射影に適用する。
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

// ValidateRegex は正規表現を独立した Postgres 文で検証する。
func (s *EPG) ValidateRegex(ctx context.Context, pattern string) error {
	if err := sqlcgen.New(s.pool).ValidateRegexPattern(ctx, pattern); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "2201B" {
			return errors.Join(store.ErrInvalidPattern, err)
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
