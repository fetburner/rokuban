package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestSupersedeFailedRecording_OnlyFailedRowIsSuperseded(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	q := sqlcgen.New(pool)

	failedID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "failed" })
	finishedID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "finished" })

	if n := dbtest.SupersedeFailedRecording(t, pool, finishedID); n != 0 {
		t.Fatalf("superseded rows for a finished recording = %d, want 0", n)
	}
	if n := dbtest.SupersedeFailedRecording(t, pool, failedID); n != 1 {
		t.Fatalf("superseded rows for the failed recording = %d, want 1", n)
	}
	got, err := q.GetRecordingByID(ctx, failedID)
	testutil.RequireNoError(t, err, "reading the superseded recording")
	if got.SupersededAt == nil {
		t.Fatalf("failed recording %d has no superseded_at after SupersedeFailedRecording", failedID)
	}
	gotFinished, err := q.GetRecordingByID(ctx, finishedID)
	testutil.RequireNoError(t, err, "reading the finished recording")
	if gotFinished.SupersededAt != nil {
		t.Fatalf("finished recording %d was superseded", finishedID)
	}
}
