package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// EncodedAssetRebuildRequest は録画 recordingID の profile に作り直し要求を作り、requested_keep を keep にする。同じ (録画, プロファイル) の行があれば keep で上書きする。keep は ms の半開区間で、空の区間だけなら失敗する。
// 要求の keep を任意に決めるテストのために、導出（RequestCutRebuild）を通さずに書く。
func EncodedAssetRebuildRequest(t testing.TB, db sqlcgen.DBTX, recordingID int64, profile string, keep []chapters.Range) {
	t.Helper()
	requestedKeep, err := keepRangesParam(keep)
	if err != nil {
		t.Fatalf("encoding rebuild request keep for recording %d profile %q: %v", recordingID, profile, err)
	}
	if err := sqlcgen.New(db).UpsertEncodedAssetRebuildRequest(t.Context(), sqlcgen.UpsertEncodedAssetRebuildRequestParams{
		RecordingID: recordingID, Profile: profile, RequestedKeep: requestedKeep,
	}); err != nil {
		t.Fatalf("upserting rebuild request for recording %d profile %q: %v", recordingID, profile, err)
	}
}
