package epgstation

import (
	"context"
	"fmt"
)

// Rule は GET /api/rules のレスポンス要素のうち、rokuban への移行
// （`rokuban import epgstation --rules`）に使うフィールドだけを持つ。
// フィールド名・入れ子構造は EPGStation の公開 REST スキーマ（l3tnun/EPGStation
// api.d.ts の Rule / AddRuleOption / RuleSearchOption / RuleReserveOption /
// ReserveSaveOption）を GitHub 上のソースから直接確認して起こした。
type Rule struct {
	// ID は EPGStation 内部のルール id。rokuban 側は rules.metadata に
	// {"epgstation":{"ruleId": ID}} として持ち、再実行時の冪等キーにする。
	ID int64 `json:"id"`
	// IsTimeSpecification が true の時刻指定ルールは programId を持たない予約を
	// 作るため rokuban では機能ごと落とす（docs/recording/reference.md）。
	// import はこれを検出して警告リストに載せ、スキップする。
	IsTimeSpecification bool              `json:"isTimeSpecification"`
	SearchOption        RuleSearchOption  `json:"searchOption"`
	ReserveOption       RuleReserveOption `json:"reserveOption"`
	// SaveOption は無指定ルールもある（TS 保存先を EPGStation の既定に委ねている
	// 場合）ので省略可能。
	SaveOption *ReserveSaveOption `json:"saveOption,omitempty"`
}

// RuleSearchOption はルールの検索条件（EPGStation api.d.ts の同名 interface）。
type RuleSearchOption struct {
	Keyword           string `json:"keyword,omitempty"`
	IgnoreKeyword     string `json:"ignoreKeyword,omitempty"`
	KeyCS             bool   `json:"keyCS,omitempty"`
	KeyRegExp         bool   `json:"keyRegExp,omitempty"`
	Name              bool   `json:"name,omitempty"`
	Description       bool   `json:"description,omitempty"`
	Extended          bool   `json:"extended,omitempty"`
	IgnoreKeyCS       bool   `json:"ignoreKeyCS,omitempty"`
	IgnoreKeyRegExp   bool   `json:"ignoreKeyRegExp,omitempty"`
	IgnoreName        bool   `json:"ignoreName,omitempty"`
	IgnoreDescription bool   `json:"ignoreDescription,omitempty"`
	IgnoreExtended    bool   `json:"ignoreExtended,omitempty"`
	GR                bool   `json:"GR,omitempty"`
	BS                bool   `json:"BS,omitempty"`
	CS                bool   `json:"CS,omitempty"`
	SKY               bool   `json:"SKY,omitempty"`
	// ChannelIDs は Mirakurun 互換の service id（= networkId*100000+serviceId。
	// internal/mirakc.SplitServiceID で分解できる）。
	ChannelIDs []int64          `json:"channelIds,omitempty"`
	Genres     []RuleGenre      `json:"genres,omitempty"`
	Times      []RuleSearchTime `json:"times,omitempty"`
	IsFree     *bool            `json:"isFree,omitempty"`
	// DurationMin/Max の単位は EPGStation ソース上に明記が無い。
	// SearchTime.range（同じ RuleSearchOption 内の兄弟フィールド）が「秒」と
	// 明記されているため、同じ単位系（秒）だろうという推測に基づき変換する
	// （internal/epgimport.buildRuleFields のコメント参照。未検証）。
	DurationMin *int64 `json:"durationMin,omitempty"`
	DurationMax *int64 `json:"durationMax,omitempty"`
	// SearchPeriods は複数期間を許すが、rokuban の rules は単一の
	// period_start_at/period_end_at しか持たない。import は先頭 1 件だけ使う。
	SearchPeriods []RuleSearchPeriod `json:"searchPeriods,omitempty"`
}

// RuleGenre はジャンル条件 1 件。SubGenre は rokuban の rule_genres が
// genre_lv1 までしか持たないため import では捨てる。
type RuleGenre struct {
	Genre    int16  `json:"genre"`
	SubGenre *int16 `json:"subGenre,omitempty"`
}

// RuleSearchTime は時間帯条件 1 件。
//
// Week は曜日ビットマスク（EPGStation: bit0=日 … bit6=土）。
// Start/Range は「時刻指定予約でない」ルール（IsTimeSpecification=false。
// import が扱うのはこちらだけ）では**時間単位**: Start は 0〜23 時の開始時刻、
// Range は 1〜23 時間の長さ（api.d.ts のコメントに明記）。
type RuleSearchTime struct {
	Start *int `json:"start,omitempty"`
	Range *int `json:"range,omitempty"`
	Week  int  `json:"week"`
}

// RuleSearchPeriod は検索対象期間 1 件（UnixtimeMS）。
type RuleSearchPeriod struct {
	StartAt int64 `json:"startAt"`
	EndAt   int64 `json:"endAt"`
}

// RuleReserveOption は予約オプション。
type RuleReserveOption struct {
	Enable         bool `json:"enable"`
	AllowEndLack   bool `json:"allowEndLack"`
	AvoidDuplicate bool `json:"avoidDuplicate"`
}

// ReserveSaveOption は保存オプション。RecordedFormat が EPGStation の
// %変数% 記法のファイル名テンプレート。
type ReserveSaveOption struct {
	RecordedFormat string `json:"recordedFormat,omitempty"`
}

// rulesResponse は GET /api/rules のレスポンス全体。ListReserves の
// reservesResponse と同じ形（rules/total）。
type rulesResponse struct {
	Rules []Rule `json:"rules"`
	Total int    `json:"total"`
}

// rulesPageLimit は 1 ページあたりの取得件数。ListReserves の
// reservesPageLimit と同じ値を踏襲する。
const rulesPageLimit = 100

// ListRules は GET /api/rules を limit/offset でページングしながら全件回収する。
// ListReserves と同じページング規約（空ページ or total 到達で打ち切り）。
func (c *Client) ListRules(ctx context.Context) ([]Rule, error) {
	var all []Rule
	offset := 0
	for {
		var page rulesResponse
		path := fmt.Sprintf("/api/rules?offset=%d&limit=%d", offset, rulesPageLimit)
		if err := c.getJSON(ctx, path, &page); err != nil {
			return nil, fmt.Errorf("listing rules (offset=%d): %w", offset, err)
		}

		all = append(all, page.Rules...)
		offset += len(page.Rules)

		if len(page.Rules) == 0 || offset >= page.Total {
			break
		}
	}
	return all, nil
}
