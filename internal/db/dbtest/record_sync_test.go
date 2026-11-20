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
	var contentLength *int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(),
		`SELECT site, recording_id, program_id, status, content_length FROM record_sync WHERE record_id = 'rec-default'`,
	).Scan(&site, &gotRecordingID, &programID, &status, &contentLength), "reading default record_sync")
	if site != "default" || gotRecordingID == nil || *gotRecordingID != recordingID || programID != 327361024000001 || status != "finished" || contentLength != nil {
		t.Fatalf("default record_sync = site %q, recording %v, program %d, status %q, content_length %v", site, gotRecordingID, programID, status, contentLength)
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

// 未紐づけの record（recording_id が NULL）と content_length の上書きを確かめる。
// 転送の分母を持つ外部 record の状態を作るときに使う。
func TestRecordSync_UnlinkedWithContentLength(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)

	length := int64(400)
	dbtest.RecordSync(t, pool, recordingID, "rec-unlinked", func(p *dbtest.RecordSyncParams) {
		p.RecordingID = nil
		p.ContentLength = &length
	})
	var gotRecordingID *int64
	var gotLength *int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(),
		`SELECT recording_id, content_length FROM record_sync WHERE record_id = 'rec-unlinked'`,
	).Scan(&gotRecordingID, &gotLength), "reading unlinked record_sync")
	if gotRecordingID != nil {
		t.Errorf("recording_id = %d, want NULL", *gotRecordingID)
	}
	if gotLength == nil || *gotLength != length {
		t.Errorf("content_length = %v, want %d", gotLength, length)
	}
}
