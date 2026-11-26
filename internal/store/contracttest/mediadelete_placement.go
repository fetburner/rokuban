package contracttest

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// runPlacementPrunes は、配置削除が有効なとき、コピーの置き場が desired の置き場と違い、desired の現行コピーが
// overlap より前に作られていれば、そのコピーを列挙することを確かめる。desired の置き場のコピーが新しすぎるときは
// 列挙せず、戻す。派生物は派生の置き場を、原本は規則の置き場を desired にする。
func runPlacementPrunes(t *testing.T, newStore NewMediaDeleteStore) {
	cases := []struct {
		name  string
		rules []store.OriginalStoreRule
		// asset は対象の資産を作り、その asset ID を返す。
		asset func(t *testing.T, f MediaDeleteFixture) int64
	}{
		{"derived copy outside the derived store", nil, func(t *testing.T, f MediaDeleteFixture) int64 {
			recordingID, _ := f.NewUntilEncodedOriginal(t)
			return f.AssetOfKind(t, recordingID, store.AssetKindEncoded)
		}},
		{"original outside the rule's store", []store.OriginalStoreRule{{Store: "derived-store"}}, func(t *testing.T, f MediaDeleteFixture) int64 {
			_, assetID := f.NewOriginal(t)
			return assetID
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := newStore(t)
			media := f.StoreWithPlacement(tc.rules, "derived-store")
			assetID := tc.asset(t, f)
			copyID := f.CopyID(t, assetID)
			f.AddCurrentCopy(t, assetID, "derived-store")
			marked, err := media.MarkDeleting(ctx, copyID, "active")
			if err != nil || !marked {
				t.Fatalf("MarkDeleting(%d): marked=%v err=%v", copyID, marked, err)
			}
			grace := time.Now().Add(-30 * 24 * time.Hour)
			pending, err := media.ListPendingDeleteAssets(ctx, grace, time.Now().Add(time.Hour), 100)
			if err != nil {
				t.Fatalf("ListPendingDeleteAssets(after overlap): %v", err)
			}
			if !containsMediaDeleteAsset(pending, copyID) {
				t.Fatalf("pending copy IDs after the overlap = %v, want copy %d", mediaDeleteAssetIDs(pending), copyID)
			}

			early := time.Now().Add(-time.Hour)
			pending, err = media.ListPendingDeleteAssets(ctx, grace, early, 100)
			if err != nil {
				t.Fatalf("ListPendingDeleteAssets(before overlap): %v", err)
			}
			if containsMediaDeleteAsset(pending, copyID) {
				t.Fatalf("pending copy IDs before the overlap = %v, want copy %d absent", mediaDeleteAssetIDs(pending), copyID)
			}
			unqualified, err := media.ListUnqualifiedDeletingAssets(ctx, grace, early, 100)
			if err != nil {
				t.Fatalf("ListUnqualifiedDeletingAssets(before overlap): %v", err)
			}
			if !containsMediaDeleteCandidate(unqualified, copyID) {
				t.Fatalf("unqualified copy IDs before the overlap = %v, want copy %d", mediaDeleteCandidateIDs(unqualified), copyID)
			}
			reverted, err := media.RevertToActive(ctx, copyID, grace, early)
			if err != nil || !reverted {
				t.Fatalf("RevertToActive(before overlap): reverted=%v err=%v", reverted, err)
			}
		})
	}
}
