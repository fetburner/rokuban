package reconciler

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

func TestGenerateContentPath(t *testing.T) {
	start := time.Date(2026, 7, 24, 21, 0, 0, 0, time.FixedZone("JST", 9*3600))
	path := generateContentPath("NHKニュース7", start, 5136)

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
			got := sanitizeContentPath(tt.input)
			if strings.Contains(got, "..") {
				t.Errorf("path traversal in sanitizeContentPath(%q): %q", tt.input, got)
			}
			if strings.HasPrefix(got, "/") {
				t.Errorf("absolute path from sanitizeContentPath(%q): %q", tt.input, got)
			}
			if strings.ContainsAny(got, "\x00") {
				t.Errorf("null byte in sanitizeContentPath(%q): %q", tt.input, got)
			}
		})
	}
}

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
		sid := int((tt.programID / 100000) % 100000)
		if sid != tt.wantSID {
			t.Errorf("programID=%d: serviceID=%d, want %d", tt.programID, sid, tt.wantSID)
		}
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
		path := generateContentPath(title, start, 1)
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
