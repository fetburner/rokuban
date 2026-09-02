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
	IsFree        *bool
	DurationMinMs *int64
	DurationMaxMs *int64
	PeriodStartAt *time.Time
	PeriodEndAt   *time.Time
	TextMatches   []TextMatch
	Services      []ServiceRef
	ChannelTypes  []string
	Genres        []int16
	Times         []TimeWindow
	// Sites は絞り込み対象の site 集合（epg_programs.site に対する OR）。
	// Compile は非空を要求する（#530）。ruler は評価対象 1 site をそのまま積む
	// （rule_sites が空でも「全サイト」を渡すのではなく、呼び出し側
	// MatchProgramIDsForRule が rule_sites との適用可否を解決したうえで
	// site 1 件に絞って渡す）。API 検索は rule_sites と同じ「空 = 全サイト」規約を
	// 保つため、リクエストの sites が空ならレジストリ全件で埋めてから渡す。
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
	// Where は AND 連結（先頭は常に p.site の述語）。先頭に AND は付けない。
	Where string
	Args  []any
	// NeedsServiceJoin が true のとき、呼び出し側は
	// JOIN epg_services s ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
	// を付ける。
	NeedsServiceJoin bool
}

// Compile は条件を SQL に落とす。
//
// c.Sites の空を「述語なし」にすると全 site を無条件に読んでしまうため、Compile は
// 非空を要求する（呼び出し側の埋め忘れに対するフェイルセーフ）。p.site = ANY($1) は
// site 始まりの複合インデックス（`(site, program_id)` 等）に乗るが、要素数の多い
// sites 配列で prepared statement が generic plan に落ちて劣化するかは未検証。
func Compile(c Conditions) (Compiled, error) {
	if len(c.Sites) == 0 {
		return Compiled{}, fmt.Errorf("sites is required")
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

	and("p.site = ANY(" + arg(c.Sites) + ")")

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

	// b は常に非空（先頭の p.site 述語が必ず and() を通るため）。
	return Compiled{Where: b.String(), Args: args, NeedsServiceJoin: needsJoin}, nil
}

func compileTextMatch(m TextMatch, arg func(any) string) (string, error) {
	rawCol, err := textTargetColumn(m.Target)
	if err != nil {
		return "", err
	}
	var clause string
	switch m.Mode {
	case "keyword":
		clause = KeywordClause(rawCol, m.Value, m.CaseSensitive, arg)
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

// KeywordClause は列 col への部分一致（LIKE '%value%'）を組む。
//
// ruler の compileTextMatch と録画検索（internal/api の recordings_query.go、
// issue #136）が共有する。共有するのはこの正規化方言だけで、テーブルや列名マップは
// 持たない --- 呼び出し側が列名（epg_programs なら "p.name"、recordings なら
// "r.title" 等）を渡す（docs/data.md §5「録画検索は rulequery を共有しない」）。
//
// caseSensitive が false なら normalize_search_text（全角→半角 + lower）を
// 両辺にかけて全角/半角の揺れを吸収する。pg_trgm の式 GIN が
// normalize_search_text(col) に乗っていれば、この形のまま加速される。
func KeywordClause(col string, value string, caseSensitive bool, arg func(any) string) string {
	if caseSensitive {
		pat := "%" + escapeLike(value) + "%"
		return col + " LIKE " + arg(pat) + " ESCAPE '\\'"
	}
	normCol := "normalize_search_text(" + col + ")"
	return normCol + " LIKE ('%' || normalize_search_text(" + arg(value) + ") || '%')"
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

// startAtJST は番組開始時刻を JST の壁時計（timestamp）に落とす式。
// 曜日と時刻の両方で使うため 1 箇所で定義する（同じ式を複数箇所に書き下すと
// 連結ミスが混入し、生成 SQL を実行しないテストではすり抜ける）。
const startAtJST = "(p.start_at AT TIME ZONE 'Asia/Tokyo')"

func compileTimeWindow(tw TimeWindow, arg func(any) string) (string, error) {
	if tw.Weekdays < 1 || tw.Weekdays > 127 {
		return "", fmt.Errorf("weekdays out of range: %d", tw.Weekdays)
	}
	if tw.StartSec < 0 || tw.StartSec > 86400 || tw.EndSec < 0 || tw.EndSec > 86400 {
		return "", fmt.Errorf("time seconds out of range")
	}

	// Postgres isodow: 1=月 … 7=日。我々のビットは bit0=月 … bit6=日 なので常に isodow-1。
	dowExpr := "(1 << (EXTRACT(ISODOW FROM " + startAtJST + ")::int - 1))"
	weekdayClause := "(" + dowExpr + " & " + arg(tw.Weekdays) + ") <> 0"

	// 時刻は JST の壁時計での「その日の 0 時からの秒」
	secExpr := "EXTRACT(EPOCH FROM " + startAtJST + "::time)::int"
	var timeClause string
	if tw.EndSec >= tw.StartSec {
		timeClause = secExpr + " >= " + arg(tw.StartSec) + " AND " + secExpr + " < " + arg(tw.EndSec)
	} else {
		// 翌日跨ぎ: start..86400 or 0..end
		timeClause = "(" + secExpr + " >= " + arg(tw.StartSec) + " OR " + secExpr + " < " + arg(tw.EndSec) + ")"
	}
	return weekdayClause + " AND " + timeClause, nil
}
