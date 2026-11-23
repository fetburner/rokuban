package store

import "testing"

func TestNormalizeRecordingStatus(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: "recording", want: "recording"},
		{raw: "finished", want: "finished"},
		{raw: "canceled", want: "canceled"},
		{raw: "failed", want: "failed"},
		{raw: "future-status", want: "failed"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := NormalizeRecordingStatus("record-1", tc.raw); got != tc.want {
				t.Errorf("NormalizeRecordingStatus(%q, %q) = %q, want %q", "record-1", tc.raw, got, tc.want)
			}
		})
	}
}
