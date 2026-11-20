package dbtest_test

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestReadProgramRows_FoundAndAbsent(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Add(time.Hour).Truncate(time.Second)
	dbtest.ProgramSnapshot(t, pool, 4321, func(p *dbtest.ProgramSnapshotParams) { p.StartAt = start })
	dbtest.ProgramIntent(t, pool, 4321, func(p *dbtest.ProgramIntentParams) { p.Action = "skip" })
	dbtest.ProgramOverrides(t, pool, 4321, func(p *dbtest.ProgramOverridesParams) { p.Overrides = []byte(`{"priority":7}`) })

	intent, ok := dbtest.ReadProgramIntent(t, pool, "default", 4321)
	if !ok || intent.Action != "skip" || intent.CreatedAt.IsZero() || intent.UpdatedAt.IsZero() {
		t.Fatalf("ReadProgramIntent = (%+v, %t), want action skip with timestamps", intent, ok)
	}
	overrides, ok := dbtest.ReadProgramOverrides(t, pool, "default", 4321)
	if !ok || string(overrides.Overrides) != `{"priority": 7}` || overrides.UpdatedAt.IsZero() {
		t.Fatalf("ReadProgramOverrides = (%+v, %t), want the written overrides", overrides, ok)
	}
	snapshot, ok := dbtest.ReadProgramSnapshot(t, pool, "default", 4321)
	if !ok || !snapshot.StartAt.Equal(start) {
		t.Fatalf("ReadProgramSnapshot = (%+v, %t), want start %v", snapshot, ok, start)
	}

	if _, ok := dbtest.ReadProgramIntent(t, pool, "default", 9999); ok {
		t.Error("ReadProgramIntent for an absent program reported a row")
	}
	if _, ok := dbtest.ReadProgramOverrides(t, pool, "default", 9999); ok {
		t.Error("ReadProgramOverrides for an absent program reported a row")
	}
	if _, ok := dbtest.ReadProgramSnapshot(t, pool, "default", 9999); ok {
		t.Error("ReadProgramSnapshot for an absent program reported a row")
	}
}
