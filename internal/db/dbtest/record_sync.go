package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// RecordSyncParams は RecordSync の上書き対象である。
type RecordSyncParams struct {
	Site      string
	ProgramID int64
}

// RecordSync は録画 recordingID に対応する finished の record_sync 行を 1 件作る。既定値は site = "default"、program_id = 327361024000001（番組スナップショットとは結び付けない）。opts で既定値を上書きする。
func RecordSync(t testing.TB, db sqlcgen.DBTX, recordingID int64, recordID string, opts ...func(*RecordSyncParams)) {
	t.Helper()
	params := RecordSyncParams{Site: "default", ProgramID: 327361024000001}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).UpsertRecordSync(t.Context(), sqlcgen.UpsertRecordSyncParams{
		Site:        params.Site,
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   params.ProgramID,
		Status:      "finished",
		Tags:        []string{},
	}); err != nil {
		t.Fatalf("upserting record_sync %q for recording %d at site %q: %v", recordID, recordingID, params.Site, err)
	}
}
