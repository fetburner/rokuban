package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// RemoveEncodeProfile は録画 recordingID の desired（encode_profiles）から profile を 1 つ外す。版（encoded）と作り直し要求は消さない。
// store の RemoveEncodedAsset は active な版を前提にし、版と要求も消すので、版の無い状態で desired だけを外すテストはこれを使う。
func RemoveEncodeProfile(t testing.TB, db sqlcgen.DBTX, recordingID int64, profile string) {
	t.Helper()
	if err := sqlcgen.New(db).RemoveRecordingEncodeProfile(t.Context(), sqlcgen.RemoveRecordingEncodeProfileParams{
		RecordingID: recordingID, Profile: profile,
	}); err != nil {
		t.Fatalf("removing encode profile %q from recording %d: %v", profile, recordingID, err)
	}
}
