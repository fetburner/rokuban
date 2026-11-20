package dbtest

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// EncodeAttemptState は録画 recordingID の profile の試行行の state（running・failed など）を返す。行が無ければ found = false を返す。
// 失敗した試行が再要求で消えたかを確かめるために読む。読み取りは試行の行だけで、判断はしない。
func EncodeAttemptState(t testing.TB, db sqlcgen.DBTX, recordingID int64, profile string) (state string, found bool) {
	t.Helper()
	row, err := sqlcgen.New(db).GetRecordingEncodeAttemptForUpdate(t.Context(), sqlcgen.GetRecordingEncodeAttemptForUpdateParams{
		RecordingID: recordingID, Profile: profile,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading encode attempt of recording %d profile %q: %v", recordingID, profile, err)
	}
	return row.State, true
}
