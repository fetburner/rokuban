package contentpath

import (
	"strings"
	"testing"
	"time"
)

func TestSanitizeComponent(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"normal", "hello", 0, "hello"},
		{"slash", "a/b", 0, "a_b"},
		{"backslash", `a\b`, 0, "a_b"},
		{"dotdot", "a..b", 0, "a_b"},
		{"null byte", "a\x00b", 0, "a_b"},
		{"control chars", "a\x01\x1fb", 0, "a__b"},
		{"colon", "title: subtitle", 0, "title_ subtitle"},
		{"windows reserved", `a*b?c"d<e>f|g`, 0, "a_b_c_d_e_f_g"},
		{"empty", "", 0, "_"},
		{"dot only", ".", 0, "_"},
		{"spaces only", "   ", 0, "_"},
		{"truncate", "abcdefghij", 5, "abcde"},
		{"japanese", "NHKニュース7", 0, "NHKニュース7"},
		{"japanese with slash", "朝/夕ニュース", 0, "朝_夕ニュース"},
		{"emoji", "🈜特番🈝", 0, "🈜特番🈝"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeComponent(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("sanitizeComponent(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestSanitizeOrEmpty(t *testing.T) {
	if got := sanitizeOrEmpty("", 0); got != "" {
		t.Errorf("sanitizeOrEmpty(\"\") = %q, want empty string (not sanitizeComponent's \"_\")", got)
	}
	if got := sanitizeOrEmpty("a/b", 0); got != "a_b" {
		t.Errorf("sanitizeOrEmpty(%q) = %q, want %q", "a/b", got, "a_b")
	}
}

func TestGenerateContentPath(t *testing.T) {
	start := time.Date(2026, 7, 24, 21, 0, 0, 0, time.FixedZone("JST", 9*3600))
	path := GenerateContentPath("NHKニュース7", start, 5136)

	if !strings.HasPrefix(path, "20260724/") {
		t.Errorf("expected date prefix, got %q", path)
	}
	if !strings.HasSuffix(path, ".m2ts") {
		t.Errorf("expected .m2ts suffix, got %q", path)
	}
	if !strings.Contains(path, "5136") {
		t.Errorf("expected serviceID in path, got %q", path)
	}
	if strings.Contains(path, "/..") || strings.Contains(path, "../") {
		t.Errorf("path traversal detected: %q", path)
	}
}

func TestGenerateContentPath_NoTraversal(t *testing.T) {
	malicious := []string{
		"../../etc/passwd",
		"/root/.ssh/authorized_keys",
		"..\\..\\windows\\system32",
		"title\x00evil",
		strings.Repeat("a", 200),
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, title := range malicious {
		path := GenerateContentPath(title, start, 1)
		if strings.Contains(path, "..") {
			t.Errorf("path traversal in %q: %q", title, path)
		}
		if strings.HasPrefix(path, "/") {
			t.Errorf("absolute path from %q: %q", title, path)
		}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			t.Errorf("expected exactly 2 path segments from %q: %q", title, path)
		}
	}
}

func TestSanitizeContentPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"traversal", "../../etc/passwd"},
		{"absolute", "/root/.ssh/keys"},
		{"null byte", "ok/title\x00evil.m2ts"},
		{"normal", "20260724/210000_NHKニュース7_5136.m2ts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeContentPath(tt.input)
			if strings.Contains(got, "..") {
				t.Errorf("path traversal in SanitizeContentPath(%q): %q", tt.input, got)
			}
			if strings.HasPrefix(got, "/") {
				t.Errorf("absolute path from SanitizeContentPath(%q): %q", tt.input, got)
			}
			if strings.ContainsAny(got, "\x00") {
				t.Errorf("null byte in SanitizeContentPath(%q): %q", tt.input, got)
			}
		})
	}
}

// TestNewData_SanitizesFreeTextButKeepsEmpty は Title/Channel/ChannelType が
// パス成分としてサニタイズされること、ただし空文字は空文字のまま
// （sanitizeComponent の "_" 昇格を受けない）ことを確認する。
func TestNewData_SanitizesFreeTextButKeepsEmpty(t *testing.T) {
	start := time.Now()

	d := NewData("朝/夕ニュース", start, "2/7", 1024, "G/R")
	if d.Title != "朝_夕ニュース" {
		t.Errorf("Title = %q, want sanitized", d.Title)
	}
	if d.Channel != "2_7" {
		t.Errorf("Channel = %q, want sanitized", d.Channel)
	}
	if d.ChannelType != "G_R" {
		t.Errorf("ChannelType = %q, want sanitized", d.ChannelType)
	}

	empty := NewData("t", start, "", 0, "")
	if empty.Channel != "" {
		t.Errorf("Channel = %q, want empty string preserved (not sanitizeComponent's \"_\")", empty.Channel)
	}
	if empty.ChannelType != "" {
		t.Errorf("ChannelType = %q, want empty string preserved", empty.ChannelType)
	}
}

// TestBuild_FieldExpansion は Data の各フィールドが text/template で正しく
// 展開されることをテーブル駆動で確認する。
func TestBuild_FieldExpansion(t *testing.T) {
	// UTC 20:00 は JST（+9h）で翌日 05:00 になる。JST 変換が実際に効いていることを
	// 確認するため、意図的に UTC/JST で日付がまたぐ時刻を選ぶ。
	startAt := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	d := NewData("ニュース7", startAt, "27", 1024, "GR")

	wantDow := dowKanji[int(startAt.In(jst).Weekday())]

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{"year", "{{.Year}}", "2026.m2ts"},
		{"shortyear", "{{.ShortYear}}", "26.m2ts"},
		{"month", "{{.Month}}", "07.m2ts"},
		{"day", "{{.Day}}", "25.m2ts"},
		{"hour", "{{.Hour}}", "05.m2ts"},
		{"min", "{{.Min}}", "00.m2ts"},
		{"sec", "{{.Sec}}", "00.m2ts"},
		{"dow", "{{.DOW}}", wantDow + ".m2ts"},
		{"title", "{{.Title}}", "ニュース7.m2ts"},
		{"channel", "{{.Channel}}", "27.m2ts"},
		{"serviceid", "{{.ServiceID}}", "1024.m2ts"},
		{"channeltype", "{{.ChannelType}}", "GR.m2ts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Build(tt.template, d)
			if err != nil {
				t.Fatalf("Build(%q): %v", tt.template, err)
			}
			if got != tt.want {
				t.Errorf("Build(%q) = %q, want %q", tt.template, got, tt.want)
			}
		})
	}
}

// TestBuild_Hierarchy はテンプレートに書かれた "/" が階層を作り、
// {{.StartAt.Format}} に書かれた "/" も同様に階層を作ることを確認する。
func TestBuild_Hierarchy(t *testing.T) {
	startAt := time.Date(2026, 7, 25, 5, 0, 0, 0, jst)
	d := NewData("ニュース7", startAt, "27", 1024, "GR")

	got, err := Build("{{.Year}}/{{.Month}}/{{.Title}}_{{.Hour}}{{.Min}}", d)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "2026/07/ニュース7_0500.m2ts"
	if got != want {
		t.Errorf("Build = %q, want %q", got, want)
	}
}

// TestBuild_ExplicitFormatSlashCreatesHierarchy は
// {{.StartAt.Format "2006/01"}} のようにテンプレートが明示的に "/" を書いた
// 場合、その "/" も階層として機能することを確認する。
func TestBuild_ExplicitFormatSlashCreatesHierarchy(t *testing.T) {
	startAt := time.Date(2026, 7, 25, 5, 0, 0, 0, jst)
	d := NewData("t", startAt, "27", 1024, "GR")

	got, err := Build(`{{.StartAt.Format "2006/01"}}/{{.Title}}`, d)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "2026/07/t.m2ts"
	if got != want {
		t.Errorf("Build = %q, want %q", got, want)
	}
}

func TestValidate_OK(t *testing.T) {
	// 空文字は Validate 自体は Parse/Execute でき、エラーにならない
	// （「未指定」として従来の固定形式にフォールバックさせるのは呼び出し側
	// ＝ api の validateRuleInput の役目で、ここでは検証しない）。
	valid := []string{
		"",
		"{{.Year}}/{{.Month}}/{{.Title}}_{{.Hour}}{{.Min}}",
		`{{.StartAt.Format "2006/01"}}/{{.Title}}`,
		"static-name-no-fields",
	}
	for _, tmpl := range valid {
		if err := Validate(tmpl); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", tmpl, err)
		}
	}
}

func TestValidate_UnknownField(t *testing.T) {
	if err := Validate("{{.Foo}}"); err == nil {
		t.Error("Validate(\"{{.Foo}}\") = nil, want error for unknown field")
	}
}

func TestValidate_MalformedSyntax(t *testing.T) {
	if err := Validate("{{.Title"); err == nil {
		t.Error("Validate(\"{{.Title\") = nil, want error for unclosed action")
	}
}
