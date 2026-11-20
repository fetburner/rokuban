package pgstore

import (
	"encoding/json"

	"github.com/fetburner/rokuban/internal/store"
)

// sqlOriginalRule は SQL 関数 desired_media_store が読む原本規則の 1 要素である。
// 形を変えると既存の SQL 関数が規則を読めなくなる。時間はマイクロ秒の older_than_us で表す。
type sqlOriginalRule struct {
	Match *sqlOriginalMatch `json:"match,omitempty"`
	Store string            `json:"store"`
}

type sqlOriginalMatch struct {
	Site            string `json:"site,omitempty"`
	OlderThanMicros int64  `json:"older_than_us,omitempty"`
}

// originalRulesJSON は policy の原本規則を、SQL の jsonb 引数の JSON にする。
// 条件が無い規則は match を省き、site が空なら site を省く。規則が無いときは空の配列になる。
// SQL 側は NULL を空の配列として読むので、零値の方針も空の配列で渡す。
func originalRulesJSON(policy store.StoragePolicy) (json.RawMessage, error) {
	rules := make([]sqlOriginalRule, 0, len(policy.Originals))
	for _, rule := range policy.Originals {
		converted := sqlOriginalRule{Store: rule.Store}
		micros := rule.OlderThan.Microseconds()
		if rule.Site != "" || micros != 0 {
			converted.Match = &sqlOriginalMatch{Site: rule.Site, OlderThanMicros: micros}
		}
		rules = append(rules, converted)
	}
	return json.Marshal(rules)
}
