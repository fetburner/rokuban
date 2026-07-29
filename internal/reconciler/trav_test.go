package reconciler

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// テンプレートと番組名はどちらもユーザー入力なので、両方から
// パストラバーサルできないことを敵対的に確認する（不変条件 7a）。
func TestBuildContentPath_AdversarialTraversal(t *testing.T) {
	sid := int32(1024)
	ch := "27"
	ct := "GR"
	base := sqlcgen.ProgramSnapshot{
		StartAt:     time.Date(2026, 8, 1, 21, 0, 0, 0, time.FixedZone("JST", 9*3600)),
		ServiceID:   &sid,
		Channel:     &ch,
		ChannelType: &ct,
	}

	cases := []struct {
		name     string
		title    string
		template string
	}{
		{"title に相対パス", "../../etc/passwd", "{{.Title}}"},
		{"title にスラッシュ", "a/b/c", "{{.Title}}"},
		{"template に相対パス", "番組", "../../{{.Title}}"},
		{"template に絶対パス", "番組", "/etc/{{.Title}}"},
		{"title に NUL とヌル文字", "a\x00b\nc", "{{.Title}}"},
		{"多重ドット", "....//....//x", "{{.Title}}"},
		{"template だけドット", "x", "{{.Title}}/../.."},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			snap := base
			snap.Title = tt.title
			got, err := buildContentPath(snap, tt.template)
			if err != nil {
				t.Fatalf("buildContentPath: %v", err)
			}

			if strings.HasPrefix(got, "/") {
				t.Errorf("絶対パスになった: %q", got)
			}
			for _, seg := range strings.Split(got, "/") {
				if seg == ".." || seg == "." || seg == "" {
					t.Errorf("危険なセグメント %q を含む: %q", seg, got)
				}
			}
			if strings.ContainsAny(got, "\x00\n\r") {
				t.Errorf("制御文字を含む: %q", got)
			}
			if !strings.HasSuffix(got, ".m2ts") {
				t.Errorf("拡張子がない: %q", got)
			}
			t.Logf("%-22s → %s", tt.name, got)
		})
	}
}

// TestBuildContentPath_ExplicitSlashInTemplateCreatesHierarchy は
// {{.StartAt.Format "2006/01"}} のようにテンプレートが明示的に "/" を書いた
// 場合は、その "/" が階層区切りとして機能することを確認する（データ由来の
// "/" は成分に閉じ込められるのと対照的に、テンプレートに書かれた "/" は
// 意図した階層を作れるという規約）。
func TestBuildContentPath_ExplicitSlashInTemplateCreatesHierarchy(t *testing.T) {
	sid := int32(1024)
	snap := sqlcgen.ProgramSnapshot{
		Title:     "番組",
		StartAt:   time.Date(2026, 8, 1, 21, 0, 0, 0, time.FixedZone("JST", 9*3600)),
		ServiceID: &sid,
	}

	got, err := buildContentPath(snap, `{{.StartAt.Format "2006/01"}}/{{.Title}}`)
	if err != nil {
		t.Fatalf("buildContentPath: %v", err)
	}
	if !strings.HasPrefix(got, "2026/08/") {
		t.Errorf("expected hierarchy from explicit template slash, got %q", got)
	}
	parts := strings.Split(got, "/")
	if len(parts) != 3 {
		t.Errorf("expected exactly 3 path segments, got %d: %q", len(parts), got)
	}
}

// TestBuildContentPath_TitleSlashDoesNotCreateHierarchy は番組名（データ由来）に
// "/" が入っていても、テンプレート自体に "/" がなければ階層にならないことを
// 確認する。
func TestBuildContentPath_TitleSlashDoesNotCreateHierarchy(t *testing.T) {
	sid := int32(1024)
	snap := sqlcgen.ProgramSnapshot{
		Title:     "前編/後編",
		StartAt:   time.Date(2026, 8, 1, 21, 0, 0, 0, time.FixedZone("JST", 9*3600)),
		ServiceID: &sid,
	}

	got, err := buildContentPath(snap, "{{.Title}}")
	if err != nil {
		t.Fatalf("buildContentPath: %v", err)
	}
	parts := strings.Split(got, "/")
	if len(parts) != 1 {
		t.Errorf("title の \"/\" が階層を作ってしまった: %q", got)
	}
}
