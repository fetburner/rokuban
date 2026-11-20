package storagename

import "testing"

func TestValidStoreName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "tokyo-2", want: true},
		{name: "", want: false},
		{name: "Tokyo", want: false},
		{name: "a_b", want: false},
		{name: "a/b", want: false},
		{name: "録画", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidStoreName(tc.name); got != tc.want {
				t.Errorf("ValidStoreName(%q) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}
