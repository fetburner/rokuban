// Package rulequery は検索・ルール評価に共通する番組条件を定義し、条件を検証する。
// 録画検索（api）が呼ぶキーワード述語 KeywordClause も持つ。
//
// SQL への変換は internal/store/pgstore が持つ。UI 検索と ruler 評価が同じ変換を通ることで、
// 「検索では出るのにルールにマッチしない」を構造的に防ぐ（docs/data/search.md §5）。
// 正規表現方言は Postgres POSIX ARE（~ / ~*）。
package rulequery

import (
	"fmt"
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
	// Validate は非空を要求する。ruler は評価対象 1 site をそのまま積む
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

// Validate は検索・ルール評価の条件がサポート対象の形か検証する。
//
// c.Sites の空を「述語なし」にすると全 site を無条件に読んでしまうため、非空を要求する
// （呼び出し側の埋め忘れに対するフェイルセーフ）。pgstore は p.site = ANY($1) で絞る。
// これは site 始まりの複合インデックス（`(site, program_id)` 等）に乗る（3 site × 10 万行の
// スクラッチ DB で 1.14ms、`p.site = $1` の 1.58ms と同等の実測）。要素数の多い
// sites 配列で prepared statement が generic plan に落ちて劣化するかは未検証。
func Validate(c Conditions) error {
	if len(c.Sites) == 0 {
		return fmt.Errorf("sites is required")
	}
	for i, match := range c.TextMatches {
		if match.Target != "name" && match.Target != "description" && match.Target != "extended" {
			return fmt.Errorf("textMatches[%d]: unknown target %q", i, match.Target)
		}
		if match.Mode != "keyword" && match.Mode != "regex" {
			return fmt.Errorf("textMatches[%d]: unknown mode %q", i, match.Mode)
		}
	}
	for i, window := range c.Times {
		if window.Weekdays < 1 || window.Weekdays > 127 {
			return fmt.Errorf("times[%d]: weekdays out of range: %d", i, window.Weekdays)
		}
		if window.StartSec < 0 || window.StartSec > 86400 || window.EndSec < 0 || window.EndSec > 86400 {
			return fmt.Errorf("times[%d]: time seconds out of range", i)
		}
	}
	return nil
}
