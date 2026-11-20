package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestManualReservation_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)

	dbtest.ProgramSnapshot(t, pool, 1234)
	dbtest.ManualReservation(t, pool, 1234)
	got, err := q.GetReservationBySiteAndProgramID(t.Context(), sqlcgen.GetReservationBySiteAndProgramIDParams{Site: "default", ProgramID: 1234})
	testutil.RequireNoError(t, err, "reading default manual reservation")
	if got.RuleID != nil || got.Base != nil {
		t.Fatalf("default manual reservation = %+v, want nil rule and base", got)
	}

	dbtest.ProgramSnapshot(t, pool, 5678, func(p *dbtest.ProgramSnapshotParams) { p.Site = "osaka" })
	dbtest.ManualReservation(t, pool, 5678, func(p *dbtest.ManualReservationParams) { p.Site = "osaka" })
	got, err = q.GetReservationBySiteAndProgramID(t.Context(), sqlcgen.GetReservationBySiteAndProgramIDParams{Site: "osaka", ProgramID: 5678})
	testutil.RequireNoError(t, err, "reading overridden manual reservation")
	if got.RuleID != nil || got.Base != nil {
		t.Fatalf("overridden manual reservation = %+v, want nil rule and base", got)
	}
}
