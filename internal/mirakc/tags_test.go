package mirakc

import "testing"

func TestProgramTag(t *testing.T) {
	tag := ProgramTag(42)
	want := "program:42"
	if tag != want {
		t.Errorf("ProgramTag(42) = %q, want %q", tag, want)
	}
}

func TestParseProgramTag(t *testing.T) {
	tests := []struct {
		tag    string
		wantID int64
		wantOK bool
	}{
		{"program:42", 42, true},
		{"program:0", 0, false},
		{"program:999999", 999999, true},
		{"program:", 0, false},
		{"program:abc", 0, false},
		{"other:tag=42", 0, false},
		{"program", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		id, ok := ParseProgramTag(tt.tag)
		if id != tt.wantID || ok != tt.wantOK {
			t.Errorf("ParseProgramTag(%q) = %d, %v; want %d, %v", tt.tag, id, ok, tt.wantID, tt.wantOK)
		}
	}
}

func TestFindProgramTag(t *testing.T) {
	tests := []struct {
		name   string
		tags   []string
		wantID int64
		wantOK bool
	}{
		{"found", []string{"foo", "program:7", "bar"}, 7, true},
		{"not found", []string{"foo", "bar"}, 0, false},
		{"empty", nil, 0, false},
		{"first match wins", []string{"program:1", "program:2"}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := FindProgramTag(tt.tags)
			if id != tt.wantID || ok != tt.wantOK {
				t.Errorf("FindProgramTag(%v) = %d, %v; want %d, %v", tt.tags, id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

// IsOurs は「自分が作った schedule だけ触る」という reconciler の不変条件の
// 判定そのもの。外部産（rokuban の tag が無い schedule）を true にしてはならない
// --- 削除ループがそれを消す。
func TestIsOurs(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want bool
	}{
		{"our tag", []string{"program:42"}, true},
		{"our tag among others", []string{"foo", "program:42", "bar"}, true},
		{"someone else's tag", []string{"foo", "bar"}, false},
		{"malformed program tag is not ours", []string{"program:abc"}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOurs(tt.tags); got != tt.want {
				t.Errorf("IsOurs(%v) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}
