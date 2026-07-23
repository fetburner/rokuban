package mirakc

import "testing"

func TestReservationTag(t *testing.T) {
	tag := ReservationTag(42)
	want := "rokuban:reservation=42"
	if tag != want {
		t.Errorf("ReservationTag(42) = %q, want %q", tag, want)
	}
}

func TestParseReservationTag(t *testing.T) {
	tests := []struct {
		tag    string
		wantID int64
		wantOK bool
	}{
		{"rokuban:reservation=42", 42, true},
		{"rokuban:reservation=0", 0, false},
		{"rokuban:reservation=999999", 999999, true},
		{"rokuban:reservation=", 0, false},
		{"rokuban:reservation=abc", 0, false},
		{"other:tag=42", 0, false},
		{"rokuban:reservation", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		id, ok := ParseReservationTag(tt.tag)
		if id != tt.wantID || ok != tt.wantOK {
			t.Errorf("ParseReservationTag(%q) = %d, %v; want %d, %v", tt.tag, id, ok, tt.wantID, tt.wantOK)
		}
	}
}

func TestFindReservationID(t *testing.T) {
	tests := []struct {
		name   string
		tags   []string
		wantID int64
		wantOK bool
	}{
		{"found", []string{"foo", "rokuban:reservation=7", "bar"}, 7, true},
		{"not found", []string{"foo", "bar"}, 0, false},
		{"empty", nil, 0, false},
		{"first match wins", []string{"rokuban:reservation=1", "rokuban:reservation=2"}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := FindReservationID(tt.tags)
			if id != tt.wantID || ok != tt.wantOK {
				t.Errorf("FindReservationID(%v) = %d, %v; want %d, %v", tt.tags, id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
