package store

import (
	"encoding/json"
	"testing"
)

func TestIsEmptyOverridesJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "empty object", raw: json.RawMessage(`{}`), want: true},
		{name: "nonempty object", raw: json.RawMessage(`{"skip":true}`), want: false},
		{name: "invalid JSON", raw: json.RawMessage(`{`), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEmptyOverridesJSON(tc.raw); got != tc.want {
				t.Errorf("IsEmptyOverridesJSON(%q) = %t, want %t", tc.raw, got, tc.want)
			}
		})
	}
}
