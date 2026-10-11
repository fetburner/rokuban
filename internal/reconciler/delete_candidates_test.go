package reconciler

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

func TestCollectDeleteCandidatesDefersRecordingAndUnknownStates(t *testing.T) {
	schedules := []mirakc.Schedule{
		{State: mirakc.ScheduleStateTracking, Program: mirakc.Program{ID: 991}, Tags: []string{mirakc.ProgramTag(991)}},
		{State: mirakc.ScheduleStateRecording, Program: mirakc.Program{ID: 992}, Tags: []string{mirakc.ProgramTag(992)}},
		{State: mirakc.ScheduleStateRescheduling, Program: mirakc.Program{ID: 993}, Tags: []string{mirakc.ProgramTag(993)}},
		{State: "future-state", Program: mirakc.Program{ID: 994}, Tags: []string{mirakc.ProgramTag(994)}},
	}

	candidates, deferred := collectDeleteCandidates(nil, schedules)
	if len(candidates) != 0 {
		for _, candidate := range candidates {
			t.Errorf("state=%q program=%d should be deferred from deletion", candidate.State, candidate.Program.ID)
		}
	}
	if deferred != len(schedules) {
		t.Errorf("deferred deletes = %d, want %d", deferred, len(schedules))
	}
}

func TestCollectDeleteCandidatesAllowsScheduledAndTerminalStates(t *testing.T) {
	schedules := []mirakc.Schedule{
		{State: mirakc.ScheduleStateScheduled, Program: mirakc.Program{ID: 995}, Tags: []string{mirakc.ProgramTag(995)}},
		{State: mirakc.ScheduleStateFinished, Program: mirakc.Program{ID: 996}, Tags: []string{mirakc.ProgramTag(996)}},
		{State: mirakc.ScheduleStateFailed, Program: mirakc.Program{ID: 997}, Tags: []string{mirakc.ProgramTag(997)}},
	}

	candidates, deferred := collectDeleteCandidates(nil, schedules)
	if len(candidates) != len(schedules) {
		t.Fatalf("delete candidates = %d, want %d", len(candidates), len(schedules))
	}
	if deferred != 0 {
		t.Errorf("deferred deletes = %d, want 0", deferred)
	}
	for i, candidate := range candidates {
		if candidate.Program.ID != schedules[i].Program.ID {
			t.Errorf("candidate[%d].Program.ID = %d, want %d", i, candidate.Program.ID, schedules[i].Program.ID)
		}
	}
}

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
