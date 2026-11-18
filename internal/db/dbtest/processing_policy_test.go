package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestProcessingPolicy_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)

	defaultID := dbtest.Recording(t, pool)
	dbtest.ProcessingPolicy(t, pool, defaultID)
	got, err := q.GetRecordingEncodePolicy(t.Context(), defaultID)
	testutil.RequireNoError(t, err, "reading default processing policy")
	if got.KeepOriginal != "always" || len(got.EncodeProfiles) != 0 || got.CmDetect {
		t.Fatalf("default processing policy = %+v, want always / {} / false", got)
	}

	overriddenID := dbtest.Recording(t, pool)
	dbtest.ProcessingPolicy(t, pool, overriddenID, func(p *dbtest.ProcessingPolicyParams) {
		p.KeepOriginal = "until_encoded"
		p.EncodeProfiles = []string{"h264"}
		p.CMDetect = true
	})
	got, err = q.GetRecordingEncodePolicy(t.Context(), overriddenID)
	testutil.RequireNoError(t, err, "reading overridden processing policy")
	if got.KeepOriginal != "until_encoded" || len(got.EncodeProfiles) != 1 || got.EncodeProfiles[0] != "h264" || !got.CmDetect {
		t.Fatalf("overridden processing policy = %+v, want until_encoded / [h264] / true", got)
	}
}
