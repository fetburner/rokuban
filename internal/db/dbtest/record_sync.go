package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// RecordSyncParams は RecordSync の上書き対象で、sqlc の生成型の別名。
type RecordSyncParams = sqlcgen.UpsertRecordSyncParams

// RecordSync は録画 recordingID に対応する record_sync 行を 1 件作る。
//
// 既定値は site = "default"、record_id = recordID、recording_id = recordingID、
// program_id = 327361024000001、status = "finished"、tags = 空。既定値は opts より
// 先に入るので、opts で任意の列を上書きできる。たとえば RecordingID を nil にすると
// 録画に紐づかない record を作り、ContentLength を渡すと転送の分母を持つ行を作る。
func RecordSync(t testing.TB, db sqlcgen.DBTX, recordingID int64, recordID string, opts ...func(*RecordSyncParams)) {
	t.Helper()
	params := RecordSyncParams{
		Site:        "default",
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   327361024000001,
		Status:      "finished",
		Tags:        []string{},
	}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).UpsertRecordSync(t.Context(), params); err != nil {
		t.Fatalf("upserting record_sync %q at site %q: %v", recordID, params.Site, err)
	}
}
