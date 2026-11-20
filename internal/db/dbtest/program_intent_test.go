package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestProgramIntent_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)

	dbtest.ProgramSnapshot(t, pool, 1234)
	dbtest.ProgramIntent(t, pool, 1234)
	got, err := q.GetProgramIntent(t.Context(), sqlcgen.GetProgramIntentParams{Site: "default", ProgramID: 1234})
	testutil.RequireNoError(t, err, "reading default program intent")
	if got.Action != "record" {
		t.Fatalf("default program intent action = %q, want record", got.Action)
	}

	dbtest.ProgramSnapshot(t, pool, 5678, func(p *dbtest.ProgramSnapshotParams) { p.Site = "osaka" })
	dbtest.ProgramIntent(t, pool, 5678, func(p *dbtest.ProgramIntentParams) {
		p.Site = "osaka"
		p.Action = "skip"
	})
	got, err = q.GetProgramIntent(t.Context(), sqlcgen.GetProgramIntentParams{Site: "osaka", ProgramID: 5678})
	testutil.RequireNoError(t, err, "reading overridden program intent")
	if got.Action != "skip" {
		t.Fatalf("overridden program intent action = %q, want skip", got.Action)
	}
}
