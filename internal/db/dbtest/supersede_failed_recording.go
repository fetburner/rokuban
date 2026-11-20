package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// SupersedeFailedRecording は failed の録画 recordingID を superseded にし、同じ放送イベントの枠を明け渡す。
// 置き換わった行数を返す。録画の放送イベントのキーは録画の行から読む。
// 対象が failed でないか、削除済み・superseded 済みなら 0 を返す。
func SupersedeFailedRecording(t testing.TB, db sqlcgen.DBTX, recordingID int64) int64 {
	t.Helper()
	q := sqlcgen.New(db)
	rec, err := q.GetRecordingByID(t.Context(), recordingID)
	if err != nil {
		t.Fatalf("reading recording %d to supersede: %v", recordingID, err)
	}
	rows, err := q.SupersedeFailedRecording(t.Context(), sqlcgen.SupersedeFailedRecordingParams{
		Site:           rec.Site,
		NetworkID:      rec.NetworkID,
		ServiceID:      rec.ServiceID,
		EventID:        rec.EventID,
		ProgramStartAt: rec.ProgramStartAt,
	})
	if err != nil {
		t.Fatalf("superseding failed recording %d: %v", recordingID, err)
	}
	return rows
}
