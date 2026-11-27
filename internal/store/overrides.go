package store

import (
	"encoding/json"
	"fmt"

	"github.com/fetburner/rokuban/internal/reservation"
)

// IsEmptyOverridesJSON は jsonb の上書きが実質空（{}）かどうかを判定する。
// reservation.Options の全フィールドが omitempty なので、json.Marshal の結果が {} であることと
// 「型付き構造体に何も設定されていない」は同値になる（新しいフィールドを足しても自動的に正しく動く）。
func IsEmptyOverridesJSON(raw json.RawMessage) bool {
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	return len(values) == 0
}

// PlanOverridesPatch は、ロック後に読んだ base と既存の上書きに patch を適用し、書くべき上書きの
// JSON と、それが空（上書きの行を消すべき）かどうかを返す。
//
// base は予約の base（ルール由来。無ければ nil）、existing は既存の上書き（無ければ nil）。
// 順序は、既存の上書きの decode、patch のマージ、JSON への変換、base と合わせた実効値の組み立て、
// keepOriginal / encodeProfiles の検証、空の判定。実効値の検証の失敗は
// *reservation.InvalidEffectiveOptionsError を含む error として返す。base または existing が
// 不正な JSON のときも error を返す。error を返すときは上書きを nil にする（呼び出し側は書かない）。
// 関数は DB にも tx にも触れない。方言の実装は、スナップショットのロックの後に base と existing を
// 読み、この結果だけを書く。振る舞いの検査は TestPlanOverridesPatch が行う。
func PlanOverridesPatch(base, existing json.RawMessage, patch reservation.OverridesPatch) (overrides json.RawMessage, empty bool, err error) {
	var current reservation.Options
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &current); err != nil {
			return nil, false, fmt.Errorf("decoding existing overrides: %w", err)
		}
	}
	merged, err := reservation.MergeOverrides(current, patch)
	if err != nil {
		return nil, false, fmt.Errorf("merging overrides: %w", err)
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		return nil, false, fmt.Errorf("encoding overrides: %w", err)
	}
	// このパッチを適用した後の実効値（base + 上書き）で検証する。リクエスト単体では組み合わせが見えない。
	effective, err := reservation.EffectiveOptions(base, payload, nil)
	if err != nil {
		return nil, false, fmt.Errorf("computing effective options: %w", err)
	}
	if err := reservation.ValidateEffectiveKeepOriginal(effective); err != nil {
		return nil, false, fmt.Errorf("validating effective options: %w", err)
	}
	return payload, IsEmptyOverridesJSON(payload), nil
}
