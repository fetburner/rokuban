package schedulesync

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
)

func TestCompareOptions(t *testing.T) {
	const programID int64 = 123
	const defaultPriority = 10

	contentPath := "custom/program.m2ts"
	filenameTemplate := "{{.YYYY}}{{.Title}}"
	priority := 12

	tests := []struct {
		name      string
		desired   reservation.Options
		observed  mirakc.Options
		tags      []string
		wantDiff  OptionsDiff
		wantOwned bool
	}{
		{
			name:      "matching options",
			observed:  mirakc.Options{Priority: defaultPriority},
			tags:      []string{mirakc.ProgramTag(programID)},
			wantOwned: true,
		},
		{
			name:      "priority mismatch",
			observed:  mirakc.Options{Priority: priority},
			tags:      []string{mirakc.ProgramTag(programID)},
			wantDiff:  OptionsDiff{Priority: true},
			wantOwned: true,
		},
		{
			name:      "program tag mismatch",
			observed:  mirakc.Options{Priority: defaultPriority},
			tags:      []string{mirakc.ProgramTag(456)},
			wantDiff:  OptionsDiff{Tag: true},
			wantOwned: true,
		},
		{
			name:    "explicit content path mismatch",
			desired: reservation.Options{ContentPath: &contentPath},
			observed: mirakc.Options{
				Priority:    defaultPriority,
				ContentPath: stringPtr("other/program.m2ts"),
			},
			tags:      []string{mirakc.ProgramTag(programID)},
			wantDiff:  OptionsDiff{ContentPath: true},
			wantOwned: true,
		},
		{
			name:    "template path is not compared",
			desired: reservation.Options{FilenameTemplate: &filenameTemplate},
			observed: mirakc.Options{
				Priority:    defaultPriority,
				ContentPath: stringPtr("old/program.m2ts"),
			},
			tags:      []string{mirakc.ProgramTag(programID)},
			wantOwned: true,
		},
		{
			name:      "external schedule is outside the comparison",
			observed:  mirakc.Options{Priority: priority},
			tags:      []string{"external"},
			wantOwned: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, owned := CompareOptions(programID, tt.desired, defaultPriority, tt.observed, tt.tags)
			if owned != tt.wantOwned {
				t.Fatalf("owned = %v, want %v", owned, tt.wantOwned)
			}
			if got != tt.wantDiff {
				t.Errorf("diff = %+v, want %+v", got, tt.wantDiff)
			}
		})
	}
}

func TestProgramEnded(t *testing.T) {
	startAt := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	const durationMs = int64(60_000)

	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "before end", now: startAt.Add(59 * time.Second), want: false},
		{name: "at end", now: startAt.Add(time.Minute), want: false},
		{name: "after end", now: startAt.Add(time.Minute + time.Nanosecond), want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProgramEnded(startAt, durationMs, tt.now); got != tt.want {
				t.Errorf("ProgramEnded() = %v, want %v", got, tt.want)
			}
		})
	}
}

func stringPtr(v string) *string { return &v }
