package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// EncodedAssetRemovalRequest は録画 recordingID の profile に版の削除要求を 1 件作る。同じ (録画, プロファイル) の行が既にあれば何もしない（要求は冪等）。
// 削除要求の行だけを作り、RemoveEncodedAsset の方針の更新や残りのコピーの検査は通さない。版を外す操作そのものを確かめるテストには使わない。
func EncodedAssetRemovalRequest(t testing.TB, db sqlcgen.DBTX, recordingID int64, profile string) {
	t.Helper()
	if err := sqlcgen.New(db).InsertEncodedAssetRemovalRequest(t.Context(), sqlcgen.InsertEncodedAssetRemovalRequestParams{
		RecordingID: recordingID, Profile: profile,
	}); err != nil {
		t.Fatalf("inserting removal request for recording %d profile %q: %v", recordingID, profile, err)
	}
}
