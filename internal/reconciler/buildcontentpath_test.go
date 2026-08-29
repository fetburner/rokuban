package reconciler

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

func TestServiceIDExtraction(t *testing.T) {
	// programId = networkId * 10_000_000_000 + serviceId * 100_000 + eventId
	tests := []struct {
		programID int64
		wantSID   int
	}{
		{100000500011234, 5000}, // networkId=10000, serviceId=5000, eventId=11234
		{327560512065535, 5120}, // networkId=32756, serviceId=5120, eventId=65535
		{327360102412345, 1024}, // networkId=32736, serviceId=1024, eventId=12345
	}
	for _, tt := range tests {
		_, sid, _ := mirakc.SplitProgramID(tt.programID)
		if sid != tt.wantSID {
			t.Errorf("programID=%d: serviceID=%d, want %d", tt.programID, sid, tt.wantSID)
		}
	}
}

// TestBuildContentPath_EmptyTemplateUsesDefaultTemplate は filenameTemplate
// が未指定のとき contentpath.DefaultTemplate を使うこと（見た目は
// 従来の固定形式のまま）を確認する。
func TestBuildContentPath_EmptyTemplateUsesDefaultTemplate(t *testing.T) {
	startAt := time.Date(2026, 7, 24, 21, 0, 0, 0, time.FixedZone("JST", 9*3600))
	serviceID := int32(5136)
	snap := sqlcgen.ProgramSnapshot{Title: "NHKニュース7", StartAt: startAt, ServiceID: serviceID}

	got, err := buildContentPath(snap, "")
	if err != nil {
		t.Fatalf("buildContentPath: %v", err)
	}
	data := contentpath.NewData(snap.Title, snap.StartAt, snap.Channel, int(serviceID), snap.ChannelType)
	want, err := contentpath.Build(contentpath.DefaultTemplate, data)
	if err != nil {
		t.Fatalf("contentpath.Build: %v", err)
	}
	if got != want {
		t.Errorf("buildContentPath with empty template = %q, want %q (DefaultTemplate)", got, want)
	}
	if got != "20260724/210000_NHKニュース7_5136.m2ts" {
		t.Errorf("buildContentPath with empty template = %q, want fixed-format literal", got)
	}
	if !strings.HasSuffix(got, ".m2ts") {
		t.Errorf("expected .m2ts suffix, got %q", got)
	}
}

// TestBuildContentPath_TextTemplateExpansion は text/template 記法で各フィールドが
// 展開され、拡張子が付くことを確認する。
func TestBuildContentPath_TextTemplateExpansion(t *testing.T) {
	startAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	serviceID := int32(1)
	snap := sqlcgen.ProgramSnapshot{Title: "t", StartAt: startAt, ServiceID: serviceID}

	got, err := buildContentPath(snap, "{{.Year}}{{.Month}}{{.Day}}_{{.Title}}")
	if err != nil {
		t.Fatalf("buildContentPath: %v", err)
	}
	if !strings.HasSuffix(got, ".m2ts") {
		t.Errorf("expected .m2ts suffix, got %q", got)
	}
	if strings.Count(got, ".m2ts") != 1 {
		t.Errorf("expected exactly one .m2ts suffix, got %q", got)
	}
}

// TestBuildContentPath_TitleNoTraversal はタイトル（データ由来）にパス
// トラバーサル的な文字列が入っても、テンプレート展開結果がトラバーサルや
// 絶対パス・NUL 文字にならないことを確認する。
func TestBuildContentPath_TitleNoTraversal(t *testing.T) {
	malicious := []string{
		"../../etc/passwd",
		"/root/.ssh/authorized_keys",
		"..\\..\\windows\\system32",
		"title\x00evil",
		"foo/../../bar",
	}
	startAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	serviceID := int32(1)

	for _, title := range malicious {
		snap := sqlcgen.ProgramSnapshot{Title: title, StartAt: startAt, ServiceID: serviceID}
		path, err := buildContentPath(snap, "{{.Title}}")
		if err != nil {
			t.Fatalf("buildContentPath(%q): %v", title, err)
		}
		if strings.Contains(path, "..") {
			t.Errorf("path traversal in %q: %q", title, path)
		}
		if strings.HasPrefix(path, "/") {
			t.Errorf("absolute path from %q: %q", title, path)
		}
		if strings.ContainsAny(path, "\x00") {
			t.Errorf("null byte in %q: %q", title, path)
		}
	}
}

// TestBuildContentPath_ErrorPropagation は text/template の構文/実行時エラー
// （未知フィールド・閉じ忘れ）が buildContentPath からそのまま返ることを
// 確認する。通常は api の validateRuleInput が作成時点で弾くが、reconciler
// 側でも推測せずエラーを伝播することを保証する。
func TestBuildContentPath_ErrorPropagation(t *testing.T) {
	startAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	serviceID := int32(1)
	snap := sqlcgen.ProgramSnapshot{Title: "t", StartAt: startAt, ServiceID: serviceID}

	if _, err := buildContentPath(snap, "{{.NoSuchField}}"); err == nil {
		t.Error("expected error for unknown template field, got nil")
	}
	if _, err := buildContentPath(snap, "{{.Title"); err == nil {
		t.Error("expected error for malformed template syntax, got nil")
	}
}
