// Package rulequery はルール条件を epg_programs に対する SQL に変換する。
//
// UI 検索と ruler 評価が同じコードを通ることで、「検索では出るのにルールに
// マッチしない」を構造的に防ぐ（docs/data.md §5 / issue #3 / M2-2）。
// 正規表現方言は Postgres POSIX ARE（~ / ~*）。
package rulequery

import (
	"fmt"
	"strings"
	"time"
)

// Conditions は 1 ルール分のマッチ条件（フラット AND + negate 付き述語）。
// 空のスライス / nil ポインタは「問わない」。
type Conditions struct {
	IsFree         *bool
	DurationMinMs  *int64
	DurationMaxMs  *int64
	PeriodStartAt  *time.Time
	PeriodEndAt    *time.Time
	TextMatches    []TextMatch
	Services       []ServiceRef
	ChannelTypes   []string
	Genres         []int16
	Times          []TimeWindow
	// Sites が空なら呼び出し側が渡す site 引数のみで絞る（ルールの「全サイト」）。
	// 非空なら、そのリストに含まれる site だけが対象（rule_sites）。
	Sites []string
}

// TextMatch は番組テキスト条件。
type TextMatch struct {
	Target        string // name | description | extended
	Mode          string // keyword | regex
	Value         string
	CaseSensitive bool
	Negate        bool
}

// ServiceRef は NID/SID。
type ServiceRef struct {
	NetworkID int32
	ServiceID int32
}

// TimeWindow は曜日ビットマスクと時刻帯。
// Weekdays: bit0=月 … bit6=日（1..127）。
// StartSec/EndSec: 0..86400。End < Start なら翌日跨ぎ。
type TimeWindow struct {
	Weekdays int
	StartSec int
	EndSec   int
}

// Compiled は epg_programs を主テーブル（エイリアス p）とする WHERE 句。
type Compiled struct {
	// Where は "TRUE" または AND 連結。先頭に AND は付けない。
	Where string
	Args  []any
	// NeedsServiceJoin が true のとき、呼び出し側は
	// JOIN epg_services s ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
	// を付ける。
	NeedsServiceJoin bool
}

// Compile は条件を SQL に落とす。
// site は評価対象サイト（N 予約の 1 サイト分）。Sites 条件がある場合は
// site がその集合に含まれることも要求する。
func Compile(site string, c Conditions) (Compiled, error) {
	if site == "" {
		return Compiled{}, fmt.Errorf("site is required")
	}

	var b strings.Builder
	args := make([]any, 0, 16)
	arg := func(v any) string {
		args = append(args, v)
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

	and("p.site = " + arg(site))

	if len(c.Sites) > 0 {
		// rule_sites 指定あり: 評価サイトがその集合に入っていること
		and(arg(site) + " = ANY(" + arg(c.Sites) + ")")
	}

	if c.IsFree != nil {
		and("p.is_free = " + arg(*c.IsFree))
	}
	if c.DurationMinMs != nil {
		and("p.duration_ms >= " + arg(*c.DurationMinMs))
	}
	if c.DurationMaxMs != nil {
		and("p.duration_ms <= " + arg(*c.DurationMaxMs))
	}
	if c.PeriodStartAt != nil {
		and("p.start_at >= " + arg(*c.PeriodStartAt))
	}
	if c.PeriodEndAt != nil {
		and("p.start_at < " + arg(*c.PeriodEndAt))
	}

	if len(c.Genres) > 0 {
		// genre_lv1 は smallint[]。重なりがあればマッチ。
		and("p.genre_lv1 && " + arg(c.Genres) + "::smallint[]")
	}

	if len(c.Services) > 0 {
		// (network_id, service_id) IN (...)
		parts := make([]string, 0, len(c.Services))
		for _, s := range c.Services {
			parts = append(parts, fmt.Sprintf("(%s, %s)", arg(s.NetworkID), arg(s.ServiceID)))
		}
		and(fmt.Sprintf("(p.network_id, p.service_id) IN (%s)", strings.Join(parts, ", ")))
	}

	needsJoin := false
	if len(c.ChannelTypes) > 0 {
		needsJoin = true
		and("s.channel_type = ANY(" + arg(c.ChannelTypes) + ")")
	}

	for i, m := range c.TextMatches {
		clause, err := compileTextMatch(m, arg)
		if err != nil {
			return Compiled{}, fmt.Errorf("textMatches[%d]: %w", i, err)
		}
		and(clause)
	}

	if len(c.Times) > 0 {
		parts := make([]string, 0, len(c.Times))
		for i, tw := range c.Times {
			clause, err := compileTimeWindow(tw, arg)
			if err != nil {
				return Compiled{}, fmt.Errorf("times[%d]: %w", i, err)
			}
			parts = append(parts, "("+clause+")")
		}
		// 複数時間帯は OR（いずれかの窓に入ればよい）
		and("(" + strings.Join(parts, " OR ") + ")")
	}

	where := b.String()
	if where == "" {
		where = "TRUE"
	}
	return Compiled{Where: where, Args: args, NeedsServiceJoin: needsJoin}, nil
}

func compileTextMatch(m TextMatch, arg func(any) string) (string, error) {
	rawCol, err := textTargetColumn(m.Target)
	if err != nil {
		return "", err
	}
	var clause string
	switch m.Mode {
	case "keyword":
		// 部分一致。非 case_sensitive は normalize_search_text（全角→半角 + lower）を
		// 両辺にかけて、検索 UI と ruler が同じ揺れ吸収を共有する。
		// pg_trgm の式 GIN が normalize_search_text(name) に乗っている。
		if m.CaseSensitive {
			pat := "%" + escapeLike(m.Value) + "%"
			clause = rawCol + " LIKE " + arg(pat) + " ESCAPE '\\'"
		} else {
			normCol := "normalize_search_text(" + rawCol + ")"
			clause = normCol + " LIKE ('%' || normalize_search_text(" + arg(m.Value) + ") || '%')"
		}
	case "regex":
		// 正規表現はユーザーが書いたパターンの意味を壊さないよう、列は生のまま。
		if m.CaseSensitive {
			clause = rawCol + " ~ " + arg(m.Value)
		} else {
			clause = rawCol + " ~* " + arg(m.Value)
		}
	default:
		return "", fmt.Errorf("unknown mode %q", m.Mode)
	}
	if m.Negate {
		return "NOT (" + clause + ")", nil
	}
	return clause, nil
}

func textTargetColumn(target string) (string, error) {
	switch target {
	case "name":
		return "p.name", nil
	case "description":
		return "p.description", nil
	case "extended":
		// jsonb をテキスト化して検索（M2-2 最小）
		return "coalesce(p.extended::text, '')", nil
	default:
		return "", fmt.Errorf("unknown target %q", target)
	}
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func compileTimeWindow(tw TimeWindow, arg func(any) string) (string, error) {
	if tw.Weekdays < 1 || tw.Weekdays > 127 {
		return "", fmt.Errorf("weekdays out of range: %d", tw.Weekdays)
	}
	if tw.StartSec < 0 || tw.StartSec > 86400 || tw.EndSec < 0 || tw.EndSec > 86400 {
		return "", fmt.Errorf("time seconds out of range")
	}

	// Postgres isodow: 1=月 … 7=日 → 我々の bit0=月 … bit6=日
	const tz = "Asia/Tokyo"
	dowExpr := "(1 << (CASE EXTRACT(ISODOW FROM p.start_at AT" + tz + "\")::int" +
		" WHEN 7 THEN 6 ELSE EXTRACT(ISODOW FROM p.start_at AT" + tz + "\")::int - 1 END))"
	weekdayClause := "(" + dowExpr + " & " + arg(tw.Weekdays) + ") <> 0"

	// 時刻は JST のその日の秒
	secExpr := "(EXTRACT(HOUR FROM p.start_at AT" + tz + "\")::int * 3600" +
		" + EXTRACT(MINUTE FROM p.start_at AT" + tz + "\")::int * 60" +
		" + EXTRACT(SECOND FROM p.start_at AT" + tz + "\")::int)"
	var timeClause string
	if tw.EndSec >= tw.StartSec {
		timeClause = secExpr + " >= " + arg(tw.StartSec) + " AND " + secExpr + " < " + arg(tw.EndSec)
	} else {
		// 翌日跨ぎ: start..86400 or 0..end
		timeClause = "(" + secExpr + " >= " + arg(tw.StartSec) + " OR " + secExpr + " < " + arg(tw.EndSec) + ")"
	}
	return weekdayClause + " AND " + timeClause, nil
}
