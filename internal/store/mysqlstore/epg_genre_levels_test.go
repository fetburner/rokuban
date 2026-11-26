package mysqlstore

import (
	"encoding/json"
	"testing"
)

// TestEpgGenreLevels は、genre_lv1 の導出（録画の復元と EPG の射影で共用する epgGenreLevels）の規則を確かめる。
// 規則は Postgres の genre_lv1_of と同じく、lv1 が 0〜15 の整数を表すものだけを数える。
func TestEpgGenreLevels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, genres, want string
	}{
		{"empty", ``, `[]`},
		{"not array", `{"lv1":3}`, `[]`},
		{"sorted and distinct", `[{"lv1":5},{"lv1":1},{"lv1":5}]`, `[1,5]`},
		{"string level", `[{"lv1":"7"}]`, `[7]`},
		{"out of range", `[{"lv1":16},{"lv1":-1},{"lv1":15}]`, `[15]`},
		{"exponent is expanded", `[{"lv1":1e1}]`, `[10]`},
		{"missing key", `[{"lv2":3}]`, `[]`},
		{"not an object", `[3]`, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := epgGenreLevels(json.RawMessage(tc.genres)); got != tc.want {
				t.Fatalf("epgGenreLevels(%s) = %s, want %s", tc.genres, got, tc.want)
			}
		})
	}
}
