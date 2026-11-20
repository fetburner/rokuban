package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCMLogoArea_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)

	dbtest.CMLogoArea(t, pool)
	area, err := q.GetCMLogoArea(t.Context(), sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
	testutil.RequireNoError(t, err, "reading default CM logo area")
	if area.X != 1180 || area.Y != 24 || area.W != 240 || area.H != 96 || area.CodedWidth != 1440 || area.CodedHeight != 1080 {
		t.Fatalf("default CM logo area = %+v, want 1180,24,240,96 in 1440x1080", area)
	}

	dbtest.CMLogoArea(t, pool, func(p *dbtest.CMLogoAreaParams) {
		p.ServiceID = 1025
		p.X = 1100
		p.CodedWidth = 1920
	})
	area, err = q.GetCMLogoArea(t.Context(), sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1025})
	testutil.RequireNoError(t, err, "reading overridden CM logo area")
	if area.X != 1100 || area.CodedWidth != 1920 {
		t.Fatalf("overridden CM logo area = %+v, want x 1100 and coded width 1920", area)
	}
}
