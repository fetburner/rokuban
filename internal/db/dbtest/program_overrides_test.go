package dbtest_test

import (
	"encoding/json"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestProgramOverrides_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)

	dbtest.ProgramSnapshot(t, pool, 1234)
	dbtest.ProgramOverrides(t, pool, 1234)
	got, err := q.GetProgramOverrides(t.Context(), sqlcgen.GetProgramOverridesParams{Site: "default", ProgramID: 1234})
	testutil.RequireNoError(t, err, "reading default program overrides")
	var defaults map[string]int
	if err := json.Unmarshal(got.Overrides, &defaults); err != nil {
		t.Fatalf("decoding default program overrides %s: %v", got.Overrides, err)
	}
	if len(defaults) != 1 || defaults["priority"] != 10 {
		t.Fatalf("default program overrides = %s, want priority 10", got.Overrides)
	}

	dbtest.ProgramSnapshot(t, pool, 5678, func(p *dbtest.ProgramSnapshotParams) { p.Site = "osaka" })
	dbtest.ProgramOverrides(t, pool, 5678, func(p *dbtest.ProgramOverridesParams) {
		p.Site = "osaka"
		p.Overrides = json.RawMessage(`{"contentPath":"recordings/test.ts"}`)
	})
	got, err = q.GetProgramOverrides(t.Context(), sqlcgen.GetProgramOverridesParams{Site: "osaka", ProgramID: 5678})
	testutil.RequireNoError(t, err, "reading overridden program overrides")
	var overridden map[string]string
	if err := json.Unmarshal(got.Overrides, &overridden); err != nil {
		t.Fatalf("decoding overridden program overrides %s: %v", got.Overrides, err)
	}
	if len(overridden) != 1 || overridden["contentPath"] != "recordings/test.ts" {
		t.Fatalf("overridden program overrides = %s, want contentPath recordings/test.ts", got.Overrides)
	}
}
