package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// EncodedAssetRemovalRequest は録画 recordingID の profile に削除の要求を置く。同じ (録画, プロファイル) の要求が既にあれば何もしない。
// 版（encoded）も desired（encode_profiles）も変えない。store の RemoveEncodedAsset は版と desired を外す操作なので、
// 要求だけを置きたい状態（削除の途中で止めた tx や、削除の到達先を見るテスト）はこれを使う。
func EncodedAssetRemovalRequest(t testing.TB, db sqlcgen.DBTX, recordingID int64, profile string) {
	t.Helper()
	if err := sqlcgen.New(db).InsertEncodedAssetRemovalRequest(t.Context(), sqlcgen.InsertEncodedAssetRemovalRequestParams{
		RecordingID: recordingID, Profile: profile,
	}); err != nil {
		t.Fatalf("inserting removal request for recording %d profile %q: %v", recordingID, profile, err)
	}
}
