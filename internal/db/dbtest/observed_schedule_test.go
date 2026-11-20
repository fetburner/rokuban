package dbtest_test

import (
	"encoding/json"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestObservedSchedule_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)

	dbtest.ObservedSchedule(t, pool, 1234)
	got, err := pgstoretest.ScheduleSync(pool).ListObserved(t.Context(), "default")
	testutil.RequireNoError(t, err, "reading default observed schedule")
	if len(got) != 1 || got[0].ProgramID != 1234 || got[0].State != "scheduled" || len(got[0].Tags) != 0 {
		t.Fatalf("default observed schedules = %+v, want program 1234 scheduled with no tags", got)
	}
	var defaults map[string]int
	if err := json.Unmarshal(got[0].Options, &defaults); err != nil {
		t.Fatalf("decoding default observed schedule options %s: %v", got[0].Options, err)
	}
	if len(defaults) != 1 || defaults["priority"] != 10 || len(got[0].FailedReason) != 0 {
		t.Fatalf("default observed schedule = %+v, want priority 10 and no failed reason", got[0])
	}
	if _, found, err := pgstoretest.ScheduleSync(pool).SnapshotAt(t.Context(), "default"); err != nil || found {
		t.Fatalf("SnapshotAt after fixture = (found %t, err %v), want no marker", found, err)
	}

	dbtest.ObservedSchedule(t, pool, 5678, func(p *dbtest.ObservedScheduleParams) {
		p.Site = "osaka"
		p.State = "failed"
		p.Options = json.RawMessage(`{"priority":20}`)
		p.Tags = []string{"program:5678"}
		p.FailedReason = json.RawMessage(`{"reason":"preempted"}`)
	})
	got, err = pgstoretest.ScheduleSync(pool).ListObserved(t.Context(), "osaka")
	testutil.RequireNoError(t, err, "reading overridden observed schedule")
	if len(got) != 1 || got[0].ProgramID != 5678 || got[0].State != "failed" || len(got[0].Tags) != 1 || got[0].Tags[0] != "program:5678" {
		t.Fatalf("overridden observed schedules = %+v, want program 5678 failed with its tag", got)
	}
	var overridden map[string]int
	if err := json.Unmarshal(got[0].Options, &overridden); err != nil {
		t.Fatalf("decoding overridden observed schedule options %s: %v", got[0].Options, err)
	}
	var reason struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(got[0].FailedReason, &reason); err != nil {
		t.Fatalf("decoding overridden observed schedule failed reason %s: %v", got[0].FailedReason, err)
	}
	if len(overridden) != 1 || overridden["priority"] != 20 || reason.Reason != "preempted" {
		t.Fatalf("overridden observed schedule options/reason = %s/%s, want priority 20/reason preempted", got[0].Options, got[0].FailedReason)
	}
}
