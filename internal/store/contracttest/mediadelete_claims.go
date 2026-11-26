package contracttest

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// placeCopy は assetID のコピーを、outside なら worker の置き場の外へ移し、そうでなければ worker の置き場に残す。
// 返す値は、置いた後のコピー ID である。
func placeCopy(t *testing.T, f MediaDeleteFixture, assetID int64, outside bool) int64 {
	t.Helper()
	if outside {
		return f.MoveCopy(t, assetID, f.BoundStore, mediaDeleteOtherStore)
	}
	return f.CopyID(t, assetID)
}

// wantCopyState は、worker の置き場のコピーなら ifOwn、外のコピーなら ifOther を返す。
func wantCopyState(own bool, ifOwn, ifOther string) string {
	if own {
		return ifOwn
	}
	return ifOther
}

// assertUntilEncodedListed は until_encoded の列挙に copyID が含まれるかを確かめる。
func assertUntilEncodedListed(t *testing.T, f MediaDeleteFixture, copyID int64, want bool) {
	t.Helper()
	rows, err := f.Store.ListUntilEncodedOriginalsToDelete(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListUntilEncodedOriginalsToDelete: %v", err)
	}
	if got := containsMediaDeleteAsset(rows, copyID); got != want {
		t.Fatalf("until-encoded candidate copy IDs = %v, copy %d listed = %v, want %v", mediaDeleteAssetIDs(rows), copyID, got, want)
	}
}

// runUntilEncodedDesiredStore は、派生物のコピーが desired の置き場の外にしか無い原本を列挙しないことを確かめる。
// 派生物の種別ごとに別の条件なので、種別ごとに確かめる。
func runUntilEncodedDesiredStore(t *testing.T, newStore NewMediaDeleteStore) {
	for _, kind := range []string{store.AssetKindEncoded, store.AssetKindThumbnail, store.AssetKindSeekTiles} {
		t.Run(kind, func(t *testing.T) {
			f := newStore(t)
			recordingID, originalID := f.NewUntilEncodedOriginal(t)
			copyID := f.CopyID(t, originalID)
			derived := f.AssetOfKind(t, recordingID, kind)
			assertUntilEncodedListed(t, f, copyID, true)

			f.MoveCopy(t, derived, f.BoundStore, mediaDeleteOtherStore)
			assertUntilEncodedListed(t, f, copyID, false)

			f.MoveCopy(t, derived, mediaDeleteOtherStore, f.BoundStore)
			assertUntilEncodedListed(t, f, copyID, true)
		})
	}
}

// runUntilEncodedCurrentGeneration は、派生物の現行世代のコピーが無ければ原本を列挙しないことを確かめる。
// 現行世代のコピーが desired の置き場の外にだけあっても同じである。
func runUntilEncodedCurrentGeneration(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	recordingID, originalID := f.NewUntilEncodedOriginal(t)
	copyID := f.CopyID(t, originalID)
	encoded := f.AssetOfKind(t, recordingID, store.AssetKindEncoded)
	f.AdvanceDerivedGeneration(t, encoded)
	assertUntilEncodedListed(t, f, copyID, false)

	f.AddCurrentCopy(t, encoded, mediaDeleteOtherStore)
	assertUntilEncodedListed(t, f, copyID, false)

	f.AddCurrentCopy(t, encoded, f.BoundStore)
	assertUntilEncodedListed(t, f, copyID, true)
}

// runUntilEncodedUsesDerivedStore は、原本の削除条件が派生物の置き場（方針の派生の置き場）で決まり、
// worker の置き場では決まらないことを確かめる。派生の置き場が無ければ何も列挙しない（全置き場には倒さない）。
func runUntilEncodedUsesDerivedStore(t *testing.T, newStore NewMediaDeleteStore) {
	cases := []struct {
		name    string
		derived string
		moveTo  string
		want    bool
	}{
		{"derived copies in the derived store", "derived-store", "derived-store", true},
		{"derived copies only in the worker store", "derived-store", "", false},
		{"no derived store configured", "", "", false},
		{"no derived store configured, copies in another store", "", "derived-store", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := newStore(t)
			recordingID, originalID := f.NewUntilEncodedOriginal(t)
			copyID := f.CopyID(t, originalID)
			if tc.moveTo != "" {
				for _, kind := range []string{store.AssetKindEncoded, store.AssetKindThumbnail, store.AssetKindSeekTiles} {
					f.MoveCopy(t, f.AssetOfKind(t, recordingID, kind), f.BoundStore, tc.moveTo)
				}
			}
			media := f.StoreWithDerived(tc.derived)
			rows, err := media.ListUntilEncodedOriginalsToDelete(ctx, 10)
			if err != nil {
				t.Fatalf("ListUntilEncodedOriginalsToDelete: %v", err)
			}
			if got := containsMediaDeleteAsset(rows, copyID); got != tc.want {
				t.Fatalf("until-encoded candidate copy IDs = %v, copy %d listed = %v, want %v", mediaDeleteAssetIDs(rows), copyID, got, tc.want)
			}
			marked, err := media.MarkUntilEncodedDeleting(ctx, recordingID, originalID, copyID)
			if err != nil {
				t.Fatalf("MarkUntilEncodedDeleting: %v", err)
			}
			if marked != tc.want {
				t.Fatalf("MarkUntilEncodedDeleting = %v, want %v", marked, tc.want)
			}
		})
	}
}

// runOldGenerationOverlap は、旧世代のコピーが、現行世代の始まりから overlap を過ぎるまで列挙されないことを確かめる。
// 現行世代の始まりは現行のコピー行の作成時刻で決まる。
func runOldGenerationOverlap(t *testing.T, newStore NewMediaDeleteStore) {
	ctx := t.Context()
	f := newStore(t)
	oldCopy, _ := f.NewOldGeneration(t)
	now := time.Now()
	early, err := f.Store.ListOldGenerationMediaAssetCopiesToDelete(ctx, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("ListOldGenerationMediaAssetCopiesToDelete(before overlap): %v", err)
	}
	if containsMediaDeleteAsset(early, oldCopy) {
		t.Fatalf("old-generation copy %d listed before the overlap cutoff: IDs = %v", oldCopy, mediaDeleteAssetIDs(early))
	}
	late, err := f.Store.ListOldGenerationMediaAssetCopiesToDelete(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListOldGenerationMediaAssetCopiesToDelete(after overlap): %v", err)
	}
	if !containsMediaDeleteAsset(late, oldCopy) {
		t.Fatalf("old-generation copy %d not listed after the overlap cutoff: IDs = %v", oldCopy, mediaDeleteAssetIDs(late))
	}
}

// runRemovedEncodedCurrentGeneration は、外した版の判定に残る版の現行世代のコピーだけを数えることを確かめる。
// 旧世代のコピーしか active でない版を残る版に数えると、現行の実体が無いのに最後の版を消してしまう。
func runRemovedEncodedCurrentGeneration(t *testing.T, newStore NewMediaDeleteStore) {
	ctx := t.Context()
	f := newStore(t)
	recordingID, removed := f.NewRemovedEncoded(t)
	kept := f.NewRetainedEncoded(t, recordingID)
	removedCopy := f.CopyID(t, removed)
	// 原本も残る版として数えられるので、原本を外して残る版だけで判定させる。
	f.SetAssetState(t, f.AssetOfKind(t, recordingID, store.AssetKindOriginal), "deleted")
	f.AdvanceDerivedGeneration(t, kept)

	listed := func() bool {
		t.Helper()
		rows, err := f.Store.ListRemovedEncodedAssetsToDelete(ctx, 10)
		if err != nil {
			t.Fatalf("ListRemovedEncodedAssetsToDelete: %v", err)
		}
		return containsMediaDeleteAsset(rows, removedCopy)
	}
	if listed() {
		t.Fatal("removed version is listed although the kept version has only an old-generation active copy")
	}
	f.AddCurrentCopy(t, kept, f.BoundStore)
	if !listed() {
		t.Fatal("removed version is not listed once the kept version has a current-generation copy")
	}
}

// runTrashDeletion は、ごみ箱の列挙が、猶予を過ぎた録画か即時削除の要求がある録画だけを返すことを確かめる。
// production の呼び出し（削除 reconcile の候補選定）は、猶予の cutoff を now - retention で渡す。
func runTrashDeletion(t *testing.T, newStore NewMediaDeleteStore) {
	cases := []struct {
		name    string
		trashed bool
		age     time.Duration
		purge   bool
		want    bool
	}{
		{"past the grace", true, 40 * 24 * time.Hour, false, true},
		{"within the grace", true, 24 * time.Hour, false, false},
		{"purge requested within the grace", true, 24 * time.Hour, true, true},
		{"not in the trash", false, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			recordingID, assetID := f.NewOriginal(t)
			if tc.trashed {
				f.TrashRecording(t, recordingID, time.Now().Add(-tc.age), tc.purge)
			}
			copyID := f.CopyID(t, assetID)
			rows, err := f.Store.ListTrashAssetsToDelete(t.Context(), time.Now().Add(-30*24*time.Hour), 100)
			if err != nil {
				t.Fatalf("ListTrashAssetsToDelete: %v", err)
			}
			if got := containsMediaDeleteAsset(rows, copyID); got != tc.want {
				t.Fatalf("trash candidate copy IDs = %v, copy %d listed = %v, want %v", mediaDeleteAssetIDs(rows), copyID, got, tc.want)
			}
		})
	}
}

// runListsIgnoreOtherStoreCopies は、列挙が worker の置き場のコピーだけを見ることを確かめる。
// 同じ論理資産が別の置き場にだけコピーを持つときは返さず、同じ状態のコピーが worker の置き場にあれば返す。
func runListsIgnoreOtherStoreCopies(t *testing.T, newStore NewMediaDeleteStore) {
	ctx := t.Context()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	cases := []struct {
		name string
		// run は資産を作り、copy を置いてから列挙し、置いた copy ID と列挙された copy ID を返す。
		run func(t *testing.T, f MediaDeleteFixture, outside bool) (copyID int64, listed []int64)
	}{
		{"trash", func(t *testing.T, f MediaDeleteFixture, outside bool) (int64, []int64) {
			recordingID, assetID := f.NewOriginal(t)
			f.TrashRecording(t, recordingID, time.Now().Add(-40*24*time.Hour), false)
			copyID := placeCopy(t, f, assetID, outside)
			rows, err := f.Store.ListTrashAssetsToDelete(ctx, cutoff, 10)
			if err != nil {
				t.Fatalf("ListTrashAssetsToDelete: %v", err)
			}
			return copyID, mediaDeleteAssetIDs(rows)
		}},
		{"until_encoded", func(t *testing.T, f MediaDeleteFixture, outside bool) (int64, []int64) {
			_, originalID := f.NewUntilEncodedOriginal(t)
			copyID := placeCopy(t, f, originalID, outside)
			rows, err := f.Store.ListUntilEncodedOriginalsToDelete(ctx, 10)
			if err != nil {
				t.Fatalf("ListUntilEncodedOriginalsToDelete: %v", err)
			}
			return copyID, mediaDeleteAssetIDs(rows)
		}},
		{"removed encoded", func(t *testing.T, f MediaDeleteFixture, outside bool) (int64, []int64) {
			_, assetID := f.NewRemovedEncoded(t)
			copyID := placeCopy(t, f, assetID, outside)
			rows, err := f.Store.ListRemovedEncodedAssetsToDelete(ctx, 10)
			if err != nil {
				t.Fatalf("ListRemovedEncodedAssetsToDelete: %v", err)
			}
			return copyID, mediaDeleteAssetIDs(rows)
		}},
		{"pending", func(t *testing.T, f MediaDeleteFixture, outside bool) (int64, []int64) {
			recordingID, assetID := f.NewOriginal(t)
			copyID := placeCopy(t, f, assetID, outside)
			f.SetAssetState(t, assetID, "deleting")
			f.TrashRecording(t, recordingID, time.Now().Add(-40*24*time.Hour), false)
			rows, err := f.Store.ListPendingDeleteAssets(ctx, cutoff, time.Time{}, 10)
			if err != nil {
				t.Fatalf("ListPendingDeleteAssets: %v", err)
			}
			return copyID, mediaDeleteAssetIDs(rows)
		}},
		{"unqualified deleting", func(t *testing.T, f MediaDeleteFixture, outside bool) (int64, []int64) {
			_, assetID := f.NewOriginal(t)
			copyID := placeCopy(t, f, assetID, outside)
			f.SetAssetState(t, assetID, "deleting")
			rows, err := f.Store.ListUnqualifiedDeletingAssets(ctx, cutoff, time.Time{}, 10)
			if err != nil {
				t.Fatalf("ListUnqualifiedDeletingAssets: %v", err)
			}
			ids := make([]int64, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			return copyID, ids
		}},
	}
	for _, tc := range cases {
		for _, side := range []struct {
			name    string
			outside bool
		}{{"copy in the worker store", false}, {"copy in another store", true}} {
			t.Run(tc.name+", "+side.name, func(t *testing.T) {
				f := newStore(t)
				copyID, listed := tc.run(t, f, side.outside)
				if got, want := containsID(listed, copyID), !side.outside; got != want {
					t.Fatalf("copy %d listed = %v (IDs %v), want %v", copyID, got, listed, want)
				}
			})
		}
	}
}

// runTransitionsIgnoreOtherStoreCopies は、遷移が worker の置き場のコピーだけを動かすことを確かめる。
// 別の置き場のコピーは状態を変えず、外した版の要求行も消さない。
func runTransitionsIgnoreOtherStoreCopies(t *testing.T, newStore NewMediaDeleteStore) {
	ctx := t.Context()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for _, side := range []struct {
		name    string
		outside bool
	}{{"copy in the worker store", false}, {"copy in another store", true}} {
		t.Run(side.name, func(t *testing.T) {
			t.Run("MarkUntilEncodedDeleting", func(t *testing.T) {
				f := newStore(t)
				recordingID, originalID := f.NewUntilEncodedOriginal(t)
				copyID := placeCopy(t, f, originalID, side.outside)
				marked, err := f.Store.MarkUntilEncodedDeleting(ctx, recordingID, originalID, copyID)
				if err != nil {
					t.Fatalf("MarkUntilEncodedDeleting: %v", err)
				}
				own := !side.outside
				if marked != own {
					t.Fatalf("MarkUntilEncodedDeleting = %v, want %v", marked, own)
				}
				if got := f.CopyState(t, copyID); got != wantCopyState(own, "deleting", "active") {
					t.Fatalf("copy state = %q, want %q", got, wantCopyState(own, "deleting", "active"))
				}
			})
			t.Run("RevertToActive", func(t *testing.T) {
				f := newStore(t)
				_, assetID := f.NewOriginal(t)
				copyID := placeCopy(t, f, assetID, side.outside)
				f.SetAssetState(t, assetID, "deleting")
				reverted, err := f.Store.RevertToActive(ctx, copyID, cutoff, time.Time{})
				if err != nil {
					t.Fatalf("RevertToActive: %v", err)
				}
				own := !side.outside
				if reverted != own {
					t.Fatalf("RevertToActive = %v, want %v", reverted, own)
				}
				if got := f.CopyState(t, copyID); got != wantCopyState(own, "active", "deleting") {
					t.Fatalf("copy state = %q, want %q", got, wantCopyState(own, "active", "deleting"))
				}
			})
			t.Run("MarkDeleting", func(t *testing.T) {
				f := newStore(t)
				_, assetID := f.NewOriginal(t)
				copyID := placeCopy(t, f, assetID, side.outside)
				marked, err := f.Store.MarkDeleting(ctx, copyID, "active")
				if err != nil {
					t.Fatalf("MarkDeleting: %v", err)
				}
				own := !side.outside
				if marked != own {
					t.Fatalf("MarkDeleting = %v, want %v", marked, own)
				}
				if got := f.CopyState(t, copyID); got != wantCopyState(own, "deleting", "active") {
					t.Fatalf("copy state = %q, want %q", got, wantCopyState(own, "deleting", "active"))
				}
			})
			t.Run("MarkDeleted", func(t *testing.T) {
				f := newStore(t)
				recordingID, assetID := f.NewRemovedEncoded(t)
				copyID := placeCopy(t, f, assetID, side.outside)
				f.SetAssetState(t, assetID, "deleting")
				if err := f.Store.MarkDeleted(ctx, copyID); err != nil {
					t.Fatalf("MarkDeleted: %v", err)
				}
				own := !side.outside
				if got := f.CopyState(t, copyID); got != wantCopyState(own, "deleted", "deleting") {
					t.Fatalf("copy state = %q, want %q", got, wantCopyState(own, "deleted", "deleting"))
				}
				if exists := f.RemovalRequestExists(t, recordingID, "h264"); exists == own {
					t.Fatalf("removal request exists = %v after MarkDeleted for a copy outside the worker store, want %v", exists, side.outside)
				}
			})
		})
	}
}

func containsID(ids []int64, id int64) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}
