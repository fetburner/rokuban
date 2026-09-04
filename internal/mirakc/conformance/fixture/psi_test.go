//go:build conformance

package fixture

import (
	"bytes"
	"testing"
	"time"
)

func TestEITDurationAndRunningStatus(t *testing.T) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	section := buildEIT(0x4E, 1, 2, 3, 0, 0, 0, 0x4E, []eitEvent{
		{EventID: 10, Start: start, Duration: UndefinedDuration, RunningStatus: 2},
	})

	// section header (3) + long-section header (5) + EIT header (6) + event id (2)
	durationOffset := 3 + 5 + 6 + 2 + 5
	if got := section[durationOffset : durationOffset+3]; !bytes.Equal(got, []byte{0xFF, 0xFF, 0xFF}) {
		t.Fatalf("undefined duration = % X, want FF FF FF", got)
	}
	runningStatusOffset := durationOffset + 3
	if got := section[runningStatusOffset] >> 5; got != 2 {
		t.Fatalf("running_status = %d, want 2", got)
	}
}

func TestEITRunningStatusDefaultsToRunning(t *testing.T) {
	section := buildEIT(0x4E, 1, 2, 3, 0, 0, 0, 0x4E, []eitEvent{{
		EventID: 10, Start: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC), Duration: time.Minute,
	}})
	durationOffset := 3 + 5 + 6 + 2 + 5
	if got := section[durationOffset+3] >> 5; got != 4 {
		t.Fatalf("default running_status = %d, want 4", got)
	}
}
