package pgstore

import (
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/rulequery"
)

// TestCompile_NoUserBytesInSQL はユーザー由来の値が SQL 文字列に一切現れないことを
// 確かめる（値はすべて $N プレースホルダ経由）。列名・演算子は whitelist なので、
// この 2 つが守られている限り注入経路は存在しない。
func TestCompile_NoUserBytesInSQL(t *testing.T) {
	const payload = `'; DROP TABLE epg_programs; --`

	out, err := compileRuleQuery(rulequery.Conditions{
		Sites:        []string{payload},
		ChannelTypes: []string{payload},
		TextMatches: []rulequery.TextMatch{
			{Target: "name", Mode: "keyword", Value: payload},
			{Target: "name", Mode: "keyword", Value: payload, CaseSensitive: true},
			{Target: "description", Mode: "regex", Value: payload},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, needle := range []string{"DROP TABLE", "--", ";"} {
		if strings.Contains(out.where, needle) {
			t.Errorf("generated SQL contains user-controlled %q: %s", needle, out.where)
		}
	}

	// 値の数だけプレースホルダが割り当てられていること
	// （Sites 1（配列 1 個で 1 プレースホルダ）+ ChannelTypes 1 + TextMatches 3 = 5）
	if len(out.args) != 5 {
		t.Errorf("len(Args) = %d, want 5 (all values parameterized)", len(out.args))
	}
	for i := range out.args {
		if !strings.Contains(out.where, "$"+itoa(i+1)) {
			t.Errorf("placeholder $%d not referenced: %s", i+1, out.where)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
