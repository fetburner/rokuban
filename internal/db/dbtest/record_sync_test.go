package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRecordSync_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)

	dbtest.RecordSync(t, pool, recordingID, "rec-default")
	var site, status string
	var gotRecordingID *int64
	var programID int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(),
		`SELECT site, recording_id, program_id, status FROM record_sync WHERE record_id = 'rec-default'`,
	).Scan(&site, &gotRecordingID, &programID, &status), "reading default record_sync")
	if site != "default" || gotRecordingID == nil || *gotRecordingID != recordingID || programID != 327361024000001 || status != "finished" {
		t.Fatalf("default record_sync = site %q, recording %v, program %d, status %q", site, gotRecordingID, programID, status)
	}

	dbtest.RecordSync(t, pool, recordingID, "rec-options", func(p *dbtest.RecordSyncParams) {
		p.Site = "site-b"
		p.ProgramID = 42
	})
	var optSite string
	var optProgramID int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(),
		`SELECT site, program_id FROM record_sync WHERE record_id = 'rec-options'`,
	).Scan(&optSite, &optProgramID), "reading overridden record_sync")
	if optSite != "site-b" || optProgramID != 42 {
		t.Fatalf("overridden record_sync = site %q, program %d, want site-b / 42", optSite, optProgramID)
	}
}
