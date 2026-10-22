package reconciler

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
)

func TestCollectDeleteCandidatesDoesNotDeferDesiredOrForeignSchedules(t *testing.T) {
	const desiredProgramID = int64(998)
	reservations := []desiredReservation{{res: reservation.SyncReservation{ProgramID: desiredProgramID}}}
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

	candidates, deferred := collectDeleteCandidates(reservations, nil, schedules)
	if len(candidates) != 0 {
		t.Errorf("delete candidates = %v, want none", candidates)
	}
	if deferred != 0 {
		t.Errorf("deferred deletes = %d, want 0 for desired and foreign schedules", deferred)
	}
}

func TestCollectDeleteCandidatesIgnoresUnresolvedBeforeDeleteAllowlist(t *testing.T) {
	const unresolvedProgramID = int64(997)
	schedules := []mirakc.Schedule{{
		State:   mirakc.ScheduleStateRecording,
		Program: mirakc.Program{ID: unresolvedProgramID},
		Tags:    []string{mirakc.ProgramTag(unresolvedProgramID)},
	}}

	candidates, deferred := collectDeleteCandidates(nil, map[int64]struct{}{unresolvedProgramID: {}}, schedules)
	if len(candidates) != 0 {
		t.Errorf("delete candidates = %v, want none", candidates)
	}
	if deferred != 0 {
		t.Errorf("deferred deletes = %d, want 0 for unresolved schedule in recording state", deferred)
	}
}
