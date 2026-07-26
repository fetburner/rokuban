package contentpath

import "testing"

// テンプレートはユーザー入力なので、ルール作成時に弾ける範囲を明示的に固定する。
// %変数% 記法から text/template に切り替えた理由がこの検証（変数名の誤りを
// 録画時ではなくルール作成時に 400 で返せる）なので、退行させない。
func TestValidate_RejectsBadTemplates(t *testing.T) {
	bad := map[string]string{
		"未知フィールド": "{{.Foo}}",
		"閉じ忘れ":    "{{.Title",
		"不正な関数":   "{{ notafunc .Title }}",
		"メソッド誤り":  `{{.StartAt.Nope}}`,
	}
	for name, tmpl := range bad {
		if err := Validate(tmpl); err == nil {
			t.Errorf("%s: Validate(%q) = nil, want error", name, tmpl)
		}
	}
	good := []string{"", "{{.Year}}/{{.Month}}/{{.Title}}", `{{.StartAt.Format "2006-01"}}/{{.Title}}`}
	for _, tmpl := range good {
		if err := Validate(tmpl); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", tmpl, err)
		}
	}
}
