package reconciler

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

func TestCollectDeleteCandidatesDoesNotDeferDesiredOrForeignSchedules(t *testing.T) {
	const desiredProgramID = int64(998)
	reservations := []desiredReservation{{res: sqlcgen.Reservation{ProgramID: desiredProgramID}}}
	schedules := []mirakc.Schedule{
		{
			State:   mirakc.ScheduleStateRecording,
			Program: mirakc.Program{ID: desiredProgramID},
			Tags:    []string{mirakc.ProgramTag(desiredProgramID)},
		},
		{
			State:   mirakc.ScheduleStateRecording,
			Program: mirakc.Program{ID: 999},
			Tags:    []string{"epgstation:rule=42"},
		},
	}

	candidates, deferred := collectDeleteCandidates(reservations, schedules)
	if len(candidates) != 0 {
		t.Errorf("delete candidates = %v, want none", candidates)
	}
	if deferred != 0 {
		t.Errorf("deferred deletes = %d, want 0 for desired and foreign schedules", deferred)
	}
}
