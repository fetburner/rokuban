package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/store"
)

// compileEPGConditions は検索とルール評価の述語を組み立てる。
// 識別子と演算子は固定し、条件の値はすべてパラメータで渡す。
func compileEPGConditions(c rulequery.Conditions) (string, []any, error) {
	if err := rulequery.Validate(c); err != nil {
		return "", nil, err
	}
	from := " FROM epg_programs p"
	if len(c.ChannelTypes) != 0 {
		from += " JOIN epg_services s ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id"
	}
	var clauses []string
	var args []any
	add := func(clause string, values ...any) {
		clauses = append(clauses, clause)
		args = append(args, values...)
	}

	add("p.site IN (" + placeholders(len(c.Sites)) + ")")
	for _, site := range c.Sites {
		args = append(args, site)
	}
	if c.IsFree != nil {
		add("p.is_free = ?", *c.IsFree)
	}
	if c.DurationMinMs != nil {
		add("p.duration_ms >= ?", *c.DurationMinMs)
	}
	if c.DurationMaxMs != nil {
		add("p.duration_ms <= ?", *c.DurationMaxMs)
	}
	if c.PeriodStartAt != nil {
		add("p.start_at >= ?", *c.PeriodStartAt)
	}
	if c.PeriodEndAt != nil {
		add("p.start_at < ?", *c.PeriodEndAt)
	}
	if len(c.Genres) > 0 {
		genres, err := json.Marshal(c.Genres)
		if err != nil {
			return "", nil, fmt.Errorf("encoding search genre levels: %w", err)
		}
		add("JSON_OVERLAPS(p.genre_lv1, CAST(? AS JSON))", string(genres))
	}
	if len(c.Services) > 0 {
		pairs := make([]string, 0, len(c.Services))
		for _, service := range c.Services {
			pairs = append(pairs, "(?, ?)")
			args = append(args, service.NetworkID, service.ServiceID)
		}
		add("(p.network_id, p.service_id) IN (" + strings.Join(pairs, ", ") + ")")
	}
	if len(c.ChannelTypes) > 0 {
		add("s.channel_type IN (" + placeholders(len(c.ChannelTypes)) + ")")
		for _, channelType := range c.ChannelTypes {
			args = append(args, channelType)
		}
	}
	for _, match := range c.TextMatches {
		clause, values := compileEPGTextMatch(match)
		add(clause, values...)
	}
	if len(c.Times) > 0 {
		windows := make([]string, 0, len(c.Times))
		for _, window := range c.Times {
			windows = append(windows, epgTimeWindow(window))
			args = append(args, window.Weekdays, window.StartSec, window.EndSec)
		}
		add("(" + strings.Join(windows, " OR ") + ")")
	}
	return from + " WHERE " + strings.Join(clauses, " AND "), args, nil
}

var epgLikeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

func compileEPGTextMatch(match rulequery.TextMatch) (string, []any) {
	var column, normalizedColumn string
	switch match.Target {
	case "name":
		column, normalizedColumn = "p.name", "p.search_name"
	case "description":
		column, normalizedColumn = "p.description", "p.search_description"
	case "extended":
		column, normalizedColumn = "COALESCE(CAST(p.extended AS CHAR CHARACTER SET utf8mb4), '')", "p.search_extended"
	}
	var clause string
	var args []any
	if match.Mode == "regex" {
		flag := "'i'"
		if match.CaseSensitive {
			flag = "'c'"
		}
		clause = "REGEXP_LIKE(" + column + ", ?, " + flag + ")"
		args = []any{match.Value}
	} else if match.CaseSensitive {
		clause = column + " COLLATE utf8mb4_0900_bin LIKE ? ESCAPE '!'"
		args = []any{"%" + epgLikeEscaper.Replace(match.Value) + "%"}
	} else {
		clause = normalizedColumn + " COLLATE utf8mb4_0900_bin LIKE LOWER(?) ESCAPE '!'"
		args = []any{"%" + epgLikeEscaper.Replace(strings.Map(narrowEPGSearchRune, match.Value)) + "%"}
	}
	if match.Negate {
		clause = "NOT (" + clause + ")"
	}
	return clause, args
}

// narrowEPGSearchRune は検索用生成列の REPLACE と同じ文字だけを半角に写す。
func narrowEPGSearchRune(r rune) rune {
	switch {
	case 'Ａ' <= r && r <= 'Ｚ', 'ａ' <= r && r <= 'ｚ', '０' <= r && r <= '９':
		return r - ('Ａ' - 'A')
	case r == '\u3000':
		return ' '
	default:
		return r
	}
}

func epgTimeWindow(window rulequery.TimeWindow) string {
	const jst = "(p.start_at + INTERVAL 9 HOUR)"
	const seconds = "FLOOR(TIME_TO_SEC(TIME(" + jst + ")) + 0.5)"
	const weekdays = "((1 << WEEKDAY(" + jst + ")) & ?) <> 0"
	operator := " AND "
	if window.EndSec < window.StartSec {
		operator = " OR "
	}
	return "(" + weekdays + " AND (" + seconds + " >= ?" + operator + seconds + " < ?))"
}

// SearchPrograms は条件に合う番組を site と番組 ID の組ごとに返す。
func (s *EPG) SearchPrograms(ctx context.Context, conditions rulequery.Conditions) ([]store.ProgramMatch, error) {
	clause, args, err := compileEPGConditions(conditions)
	if err != nil {
		return nil, err
	}
	return queryEPGRows(ctx, s.db, "program matches",
		"SELECT "+programMatchColumns+clause+" ORDER BY p.program_id, p.site", scanProgramMatch, args...)
}

// MatchProgramIDsForRule は評価対象の site でルールに合う番組の ID を返す。
func (s *EPG) MatchProgramIDsForRule(ctx context.Context, site string, ruleID int64) ([]int64, error) {
	conditions, err := s.loadRuleConditions(ctx, ruleID)
	if err != nil {
		return nil, err
	}
	if len(conditions.Sites) > 0 && !slices.Contains(conditions.Sites, site) {
		return nil, nil
	}
	conditions.Sites = []string{site}
	clause, args, err := compileEPGConditions(conditions)
	if err != nil {
		return nil, err
	}
	return queryEPGRows(ctx, s.db, "program IDs matching rule", "SELECT p.program_id"+clause,
		func(rows *sql.Rows) (int64, error) {
			var id int64
			err := rows.Scan(&id)
			return id, err
		}, args...)
}

// ValidateRegex は ICU が受理する正規表現かを独立した MySQL 文で検証する。
func (s *EPG) ValidateRegex(ctx context.Context, pattern string) error {
	var matched bool
	err := s.db.QueryRowContext(ctx, "SELECT REGEXP_LIKE('', ?, 'c')", pattern).Scan(&matched)
	if err == nil {
		return nil
	}
	var dbErr *mysql.MySQLError
	if errors.As(err, &dbErr) {
		switch dbErr.Number {
		case 3685, // ER_REGEXP_ILLEGAL_ARGUMENT
			3688, 3689, 3690, 3691, 3692, 3693, 3694, 3695, 3696, 3697, 3698, // ICU の構文・機能の制約
			3702: // ER_REGEXP_INVALID_CAPTURE_GROUP_NAME
			return invalidRegexError{dbErr}
		}
	}
	return fmt.Errorf("validating ICU regular expression: %w", err)
}

type invalidRegexError struct{ err error }

func (e invalidRegexError) Error() string        { return e.err.Error() }
func (e invalidRegexError) Unwrap() error        { return e.err }
func (e invalidRegexError) Is(target error) bool { return target == store.ErrInvalidPattern }

// loadRuleConditions は親と子の条件を 1 文で読み、同じスナップショットから条件を作る。
func (s *EPG) loadRuleConditions(ctx context.Context, ruleID int64) (rulequery.Conditions, error) {
	var c rulequery.Conditions
	var sites, services, channelTypes, genres, texts, windows []byte
	err := s.db.QueryRowContext(ctx, `
SELECT r.is_free, r.duration_min_ms, r.duration_max_ms, r.period_start_at, r.period_end_at,
       COALESCE((SELECT JSON_ARRAYAGG(site) FROM rule_sites WHERE rule_id = r.id), JSON_ARRAY()),
       COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('NetworkID', network_id, 'ServiceID', service_id))
                 FROM rule_services WHERE rule_id = r.id), JSON_ARRAY()),
       COALESCE((SELECT JSON_ARRAYAGG(channel_type) FROM rule_channel_types WHERE rule_id = r.id), JSON_ARRAY()),
       COALESCE((SELECT JSON_ARRAYAGG(genre_lv1) FROM rule_genres WHERE rule_id = r.id), JSON_ARRAY()),
       COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('Target', target, 'Mode', mode, 'Value', value,
                                                 'CaseSensitive', IF(case_sensitive, CAST('true' AS JSON), CAST('false' AS JSON)),
                                                 'Negate', IF(negate, CAST('true' AS JSON), CAST('false' AS JSON))))
                 FROM rule_text_matches WHERE rule_id = r.id), JSON_ARRAY()),
       COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('Weekdays', weekdays, 'StartSec', start_sec, 'EndSec', end_sec))
                 FROM rule_times WHERE rule_id = r.id), JSON_ARRAY())
FROM rules r WHERE r.id = ?`, ruleID).Scan(&c.IsFree, &c.DurationMinMs, &c.DurationMaxMs, &c.PeriodStartAt, &c.PeriodEndAt,
		&sites, &services, &channelTypes, &genres, &texts, &windows)
	if errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("rule %d: %w", ruleID, store.ErrNotFound)
	}
	if err != nil {
		return c, fmt.Errorf("getting conditions for rule %d: %w", ruleID, err)
	}
	for _, field := range []struct {
		data []byte
		dest any
	}{
		{sites, &c.Sites}, {services, &c.Services}, {channelTypes, &c.ChannelTypes},
		{genres, &c.Genres}, {texts, &c.TextMatches}, {windows, &c.Times},
	} {
		if err := json.Unmarshal(field.data, field.dest); err != nil {
			return c, fmt.Errorf("decoding conditions for rule %d: %w", ruleID, err)
		}
	}
	return c, nil
}
