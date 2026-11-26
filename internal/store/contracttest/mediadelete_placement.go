package contracttest

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// runPlacementPrunes は、配置削除の判定を表で確かめる。コピーの置き場が desired の置き場と違い、desired の現行コピーが
// overlap より前に作られていれば、そのコピーは列挙される（want）。desired が変わるのは、原本の規則の先勝ち・
// site・older_than と、配置削除の有無である。派生物は派生の置き場を desired にする。
func runPlacementPrunes(t *testing.T, newStore NewMediaDeleteStore) {
	derivedAsset := func(t *testing.T, f MediaDeleteFixture) int64 {
		recordingID, _ := f.NewUntilEncodedOriginal(t)
		return f.AssetOfKind(t, recordingID, store.AssetKindEncoded)
	}
	originalAsset := func(t *testing.T, f MediaDeleteFixture) int64 {
		_, assetID := f.NewOriginal(t)
		return assetID
	}
	cases := []struct {
		name    string
		rules   []store.OriginalStoreRule
		enabled bool
		// asset は対象の資産を作り、その asset ID を返す。
		asset func(t *testing.T, f MediaDeleteFixture) int64
		// want は、overlap を過ぎたときに desired のコピーを理由として列挙されるかを表す。
		want bool
	}{
		{"derived copy outside the derived store", nil, true, derivedAsset, true},
		{"original outside the rule's store", []store.OriginalStoreRule{{Store: "derived-store"}}, true, originalAsset, true},
		// 先頭の規則は site が一致しないので飛ばし、次の規則（派生の置き場）に落ちる。
		{"first rule's site differs from the recording's", []store.OriginalStoreRule{{Site: "no-such-site", Store: "elsewhere"}, {Store: "derived-store"}}, true, originalAsset, true},
		// 先頭の規則は older_than がまだ経っていないので、次の規則に落ちる。
		{"first rule's older-than has not passed", []store.OriginalStoreRule{{OlderThan: 100000 * time.Hour, Store: "elsewhere"}, {Store: "derived-store"}}, true, originalAsset, true},
		// 配置削除が無効なら、規則が一致しても配置の腕は効かない。
		{"placement disabled", []store.OriginalStoreRule{{Store: "derived-store"}}, false, originalAsset, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := newStore(t)
			media := f.StoreWithPlacement(tc.rules, "derived-store", tc.enabled)
			assetID := tc.asset(t, f)
			copyID := f.CopyID(t, assetID)
			f.AddCurrentCopy(t, assetID, "derived-store")
			marked, err := media.MarkDeleting(ctx, copyID, "active")
			if err != nil || !marked {
				t.Fatalf("MarkDeleting(%d): marked=%v err=%v", copyID, marked, err)
			}
			grace := time.Now().Add(-30 * 24 * time.Hour)
			late := time.Now().Add(time.Hour)
			early := time.Now().Add(-time.Hour)
			pending, err := media.ListPendingDeleteAssets(ctx, grace, late, 100)
			if err != nil {
				t.Fatalf("ListPendingDeleteAssets(after overlap): %v", err)
			}
			if got := containsMediaDeleteAsset(pending, copyID); got != tc.want {
				t.Fatalf("pending copy IDs after the overlap = %v, copy %d listed = %v, want %v", mediaDeleteAssetIDs(pending), copyID, got, tc.want)
			}

			// 列挙されない間は deleting のまま戻せる（条件から外れている）。戻す cutoff は、判定が外れる側を選ぶ。
			unqualifiedAt := late
			if tc.want {
				unqualifiedAt = early
			}
			unqualified, err := media.ListUnqualifiedDeletingAssets(ctx, grace, unqualifiedAt, 100)
			if err != nil {
				t.Fatalf("ListUnqualifiedDeletingAssets: %v", err)
			}
			if !containsMediaDeleteCandidate(unqualified, copyID) {
				t.Fatalf("unqualified copy IDs = %v, want copy %d", mediaDeleteCandidateIDs(unqualified), copyID)
			}
			reverted, err := media.RevertToActive(ctx, copyID, grace, unqualifiedAt)
			if err != nil || !reverted {
				t.Fatalf("RevertToActive: reverted=%v err=%v", reverted, err)
			}
		})
	}
}
