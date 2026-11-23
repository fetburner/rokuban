package store

import "encoding/json"

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
