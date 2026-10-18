// Package rulequery は検索・ルール評価に共通する番組条件を定義し、条件を検証する。
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
	// Sites は絞り込み対象の site 集合。検索 API は空なら登録済み全 site を入れ、
	// ruler はルールの適用可否を確認したうえで評価対象 site 1 件を入れる。
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
