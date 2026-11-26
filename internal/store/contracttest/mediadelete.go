package contracttest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// MediaDeleteFixture はメディア削除契約と、その状態を作るための実装別フックをまとめる。
// フックは状態を作るだけで、store の契約ではない（SQL で直接書いてよい）。
type MediaDeleteFixture struct {
	// Store は worker の置き場（BoundStore）に束縛し、派生物の置き場も同じ置き場とする。
	Store store.MediaDelete
	// StoreWithDerived は BoundStore に束縛し、派生物の置き場を derivedStore にする（"" なら未設定）。
	StoreWithDerived func(derivedStore string) store.MediaDelete
	// StoreWithPlacement は BoundStore に束縛し、配置削除を有効にした方針（原本の規則と派生の置き場）で作る。
	StoreWithPlacement func(originalRules []store.OriginalStoreRule, derivedStore string) store.MediaDelete
	// BoundStore は Store が束縛している置き場の名前である。
	BoundStore string
	Serving    store.MediaServing
	Catalog    store.Catalog
	// MarkMissing は copyID の実体が無いことを観測として記録する。
	MarkMissing func(t *testing.T, copyID int64)
	// NewOriginal は録画を 1 件作り、その原本を BoundStore の active コピーで持たせる。
	NewOriginal func(t *testing.T) (recordingID, assetID int64)
	// NewOldGeneration は派生物の旧世代コピー（BoundStore・active）と、その現行世代のコピーを作る。
	NewOldGeneration func(t *testing.T) (oldCopyID, currentCopyID int64)
	// NewRetainedEncoded は録画に残る版（別の profile）の encoded を作り、その asset ID を返す。
	NewRetainedEncoded func(t *testing.T, recordingID int64) (assetID int64)
	SetAssetState      func(t *testing.T, assetID int64, state string)
	// NewUntilEncodedOriginal は until_encoded の削除条件を満たす録画を作る（派生物は BoundStore にある）。
	NewUntilEncodedOriginal func(t *testing.T) (recordingID, assetID int64)
	// AssetOfKind は録画の kind の asset ID を返す（encoded が複数あるときは使わない）。
	AssetOfKind func(t *testing.T, recordingID int64, kind string) int64
	// BeginUntilEncodedInvalidation は録画の行をロックし、作り直し要求を書いた tx を開いたままにする（commit を返す）。
	BeginUntilEncodedInvalidation func(t *testing.T, recordingID int64) (commit func())
	// BeginCopyDeletion は copyID を deleted にする tx を開いたままにする（commit を返す）。
	BeginCopyDeletion func(t *testing.T, copyID int64) (commit func())
	NewDeletedAsset   func(t *testing.T) (recordingID, assetID int64)
	NewRemovedEncoded func(t *testing.T) (recordingID, assetID int64)
	// TrashRecording は録画をごみ箱に入れる。purgeRequested なら即時削除の要求も書く。
	TrashRecording func(t *testing.T, recordingID int64, deletedAt time.Time, purgeRequested bool)
	// CopyID は assetID の BoundStore のコピー ID を返す。
	CopyID func(t *testing.T, assetID int64) int64
	// CopyState はコピー行の状態を返す。
	CopyState func(t *testing.T, copyID int64) string
	// MoveCopy は assetID のコピーを from の置き場から to の置き場へ移し、移した後のコピー ID を返す。
	MoveCopy func(t *testing.T, assetID int64, from, to string) int64
	// AdvanceDerivedGeneration は派生物の現行世代を 1 つ進める（新しい世代のコピーはまだ無い）。
	AdvanceDerivedGeneration func(t *testing.T, assetID int64)
	// AddCurrentCopy は assetID の現行世代の active コピーを storeName に作り、そのコピー ID を返す。
	AddCurrentCopy func(t *testing.T, assetID int64, storeName string) int64
	// RemovalRequestExists は削除要求の行があるかを返す。ListRemovedEncodedAssetsToDelete は
	// deleted の asset を返さないので、MarkDeleted が要求の行を消したかを確かめられない。
	RemovalRequestExists func(t *testing.T, recordingID int64, profile string) bool
	// WaitForLockWaiter は、operation が行ロックの FOR UPDATE で待つまで待つ。
	WaitForLockWaiter func(t *testing.T, operation string)
}

// mediaDeleteOtherStore は、worker の置き場以外の置き場の名前である。
const mediaDeleteOtherStore = "other-store"

// NewMediaDeleteStore はメディア削除契約の実装とフィクスチャを作る。
type NewMediaDeleteStore func(t *testing.T) MediaDeleteFixture

// RunMediaDelete はメディア削除の契約を実行する。
func RunMediaDelete(t *testing.T, newStore NewMediaDeleteStore) {
	t.Helper()
	t.Run("until-encoded deletion requires present derived copies", func(t *testing.T) { runUntilEncodedPresence(t, newStore) })
	t.Run("until-encoded deletion requires derived copies in the desired store", func(t *testing.T) {
		runUntilEncodedDesiredStore(t, newStore)
	})
	t.Run("until-encoded deletion requires a current-generation derived copy", func(t *testing.T) {
		runUntilEncodedCurrentGeneration(t, newStore)
	})
	t.Run("until-encoded deletion uses the derived store, not the worker store", func(t *testing.T) {
		runUntilEncodedUsesDerivedStore(t, newStore)
	})
	t.Run("old-generation deletion requires a present current copy", func(t *testing.T) { runOldGenerationPresence(t, newStore) })
	t.Run("old-generation deletion waits for the overlap cutoff", func(t *testing.T) { runOldGenerationOverlap(t, newStore) })
	t.Run("removed encoded deletion requires another present version", func(t *testing.T) { runRemovedEncodedPresence(t, newStore) })
	t.Run("removed encoded deletion counts only current-generation copies", func(t *testing.T) {
		runRemovedEncodedCurrentGeneration(t, newStore)
	})
	t.Run("trash deletion takes recordings past the grace or with a purge request", func(t *testing.T) {
		runTrashDeletion(t, newStore)
	})
	t.Run("placement deletion prunes a copy once its desired copy is past the overlap", func(t *testing.T) {
		runPlacementPrunes(t, newStore)
	})
	t.Run("list methods ignore copies in other stores", func(t *testing.T) { runListsIgnoreOtherStoreCopies(t, newStore) })
	t.Run("transitions ignore copies in other stores", func(t *testing.T) { runTransitionsIgnoreOtherStoreCopies(t, newStore) })
	t.Run("until-encoded transition waits for and rechecks recording writes", func(t *testing.T) {
		runUntilEncodedSerialization(t, newStore)
	})
	t.Run("mark deleting waits for a concurrent copy transition and does not resurrect", func(t *testing.T) {
		runMarkDeletingWaitsForCopyTransition(t, newStore)
	})
	t.Run("revert does not restore a deleted asset", func(t *testing.T) {
		runDeletedAssetIsNotReverted(t, newStore)
	})
	t.Run("mark deleted clears the removal request", func(t *testing.T) {
		runMarkDeletedClearsRemovalRequest(t, newStore)
	})
}

func runMarkDeletedClearsRemovalRequest(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	recordingID, assetID := f.NewRemovedEncoded(t)
	copyID := f.CopyID(t, assetID)
	removed, err := f.Store.ListRemovedEncodedAssetsToDelete(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRemovedEncodedAssetsToDelete(before delete): %v", err)
	}
	if !containsMediaDeleteAsset(removed, copyID) {
		t.Fatalf("ListRemovedEncodedAssetsToDelete copy IDs = %v, want copy %d", mediaDeleteAssetIDs(removed), copyID)
	}
	if marked, err := f.Store.MarkDeleting(context.Background(), copyID, "active"); err != nil || !marked {
		t.Fatalf("MarkDeleting(%d): marked=%v err=%v", copyID, marked, err)
	}
	if err := f.Store.MarkDeleted(context.Background(), copyID); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	assertMediaAssetDeleted(t, f, recordingID, assetID, store.AssetKindEncoded, "h264")
	if f.RemovalRequestExists(t, recordingID, "h264") {
		t.Fatalf("removal request for deleted asset %d remains", assetID)
	}
}

func runUntilEncodedSerialization(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID, assetID := f.NewUntilEncodedOriginal(t)
	copyID := f.CopyID(t, assetID)
	candidates, err := f.Store.ListUntilEncodedOriginalsToDelete(ctx, 10)
	if err != nil {
		t.Fatalf("listing eligible until_encoded originals: %v", err)
	}
	if !containsMediaDeleteAsset(candidates, copyID) {
		t.Fatalf("until_encoded candidates = %+v, want copy %d", candidates, copyID)
	}

	commit := f.BeginUntilEncodedInvalidation(t, recordingID)
	var commitOnce sync.Once
	commitBlocker := func() { commitOnce.Do(commit) }
	defer commitBlocker()
	result := make(chan struct {
		marked bool
		err    error
	}, 1)
	go func() {
		marked, err := f.Store.MarkUntilEncodedDeleting(ctx, recordingID, assetID, copyID)
		result <- struct {
			marked bool
			err    error
		}{marked: marked, err: err}
	}()
	f.WaitForLockWaiter(t, "MarkUntilEncodedDeleting")
	commitBlocker()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("MarkUntilEncodedDeleting after rebuild request: %v", got.err)
		}
		if got.marked {
			t.Fatal("MarkUntilEncodedDeleting applied after a rebuild request committed, want it to recheck eligibility")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MarkUntilEncodedDeleting did not finish after the recording lock was released")
	}
	active, err := f.Serving.GetActiveOriginalMediaAsset(ctx, recordingID)
	if err != nil || active.ID != assetID {
		t.Fatalf("GetActiveOriginalMediaAsset(%d) ID = %d, err = %v, want ID %d and nil error", recordingID, active.ID, err, assetID)
	}
}

func runDeletedAssetIsNotReverted(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	recordingID, assetID := f.NewDeletedAsset(t)
	copyID := f.CopyID(t, assetID)
	updated, err := f.Store.RevertToActive(context.Background(), copyID, time.Now(), time.Time{})
	if err != nil {
		t.Fatalf("RevertToActive(deleted) error = %v", err)
	}
	if updated {
		t.Fatal("RevertToActive(deleted) updated the asset, want no change")
	}
	assertMediaAssetDeleted(t, f, recordingID, assetID, store.AssetKindOriginal, "")
}

// runMarkDeletingWaitsForCopyTransition は、別の tx がコピーを deleted にしている間は待ち、
// commit 後にその結果を見て、deleted を deleting に戻さない。
func runMarkDeletingWaitsForCopyTransition(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	ctx := context.Background()
	_, assetID := f.NewOriginal(t)
	copyID := f.CopyID(t, assetID)
	commit := f.BeginCopyDeletion(t, copyID)
	var commitOnce sync.Once
	commitBlocker := func() { commitOnce.Do(commit) }
	defer commitBlocker()

	done := make(chan error, 1)
	go func() {
		_, err := f.Store.MarkDeleting(ctx, copyID, "active")
		done <- err
	}()
	f.WaitForLockWaiter(t, "MarkDeleting")
	select {
	case err := <-done:
		t.Fatalf("MarkDeleting returned (%v) before the concurrent transition committed, want it to wait", err)
	default:
	}
	commitBlocker()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("MarkDeleting after commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MarkDeleting did not finish after the concurrent transition committed")
	}
	if got := f.CopyState(t, copyID); got != "deleted" {
		t.Fatalf("copy state = %q, want deleted (MarkDeleting must not move a deleted copy back to deleting)", got)
	}
}

func assertMediaAssetDeleted(t *testing.T, f MediaDeleteFixture, recordingID, assetID int64, kind, profile string) {
	t.Helper()
	document, err := f.Catalog.Export(context.Background())
	if err != nil {
		t.Fatalf("Catalog.Export while checking deleted asset %d: %v", assetID, err)
	}
	var deletedAsset *catalogdoc.MediaAsset
	for i := range document.MediaAssets {
		if document.MediaAssets[i].ID == assetID {
			deletedAsset = &document.MediaAssets[i]
			break
		}
	}
	if deletedAsset == nil {
		t.Fatalf("Catalog.Export.MediaAssets has no asset %d, want RecordingID %d, State \"deleted\" and DeletedAt set", assetID, recordingID)
	}
	if deletedAsset.RecordingID != recordingID {
		t.Fatalf("asset %d RecordingID = %d, want %d", assetID, deletedAsset.RecordingID, recordingID)
	}
	if deletedAsset.State != "deleted" {
		t.Fatalf("asset %d State = %q, want %q", assetID, deletedAsset.State, "deleted")
	}
	if deletedAsset.DeletedAt == nil {
		t.Fatalf("asset %d DeletedAt = nil, want set", assetID)
	}
	copyID := f.CopyID(t, assetID)

	var servingErr error
	if kind == store.AssetKindEncoded {
		_, servingErr = f.Serving.GetEncodedMediaAssetForServing(context.Background(), recordingID, profile)
	} else {
		_, servingErr = f.Serving.GetOriginalMediaAssetForServing(context.Background(), recordingID)
	}
	if !errors.Is(servingErr, store.ErrNotFound) {
		t.Fatalf("serving lookup for deleted %s asset %d = %v, want ErrNotFound", kind, assetID, servingErr)
	}
	cutoff := time.Now().Add(time.Hour)
	keepCurrentCopies := time.Now().Add(time.Hour)
	pending, err := f.Store.ListPendingDeleteAssets(context.Background(), cutoff, keepCurrentCopies, 100)
	if err != nil {
		t.Fatalf("ListPendingDeleteAssets when checking deleted asset %d: %v", assetID, err)
	}
	unqualified, err := f.Store.ListUnqualifiedDeletingAssets(context.Background(), cutoff, keepCurrentCopies, 100)
	if err != nil {
		t.Fatalf("ListUnqualifiedDeletingAssets when checking deleted asset %d: %v", assetID, err)
	}
	if containsMediaDeleteAsset(pending, copyID) || containsMediaDeleteCandidate(unqualified, copyID) {
		t.Fatalf("deleted asset %d copy %d remains in a deleting queue: ListPendingDeleteAssets IDs = %v, ListUnqualifiedDeletingAssets IDs = %v, want neither to contain it", assetID, copyID, mediaDeleteAssetIDs(pending), mediaDeleteCandidateIDs(unqualified))
	}
}

func containsMediaDeleteCandidate(assets []store.MediaDeleteRevertCandidate, id int64) bool {
	for _, asset := range assets {
		if asset.ID == id {
			return true
		}
	}
	return false
}

func containsMediaDeleteAsset(assets []store.MediaDeleteAsset, id int64) bool {
	for _, asset := range assets {
		if asset.ID == id {
			return true
		}
	}
	return false
}

// mediaDeleteAssetIDs は失敗メッセージ用に asset の ID を並べる。
func mediaDeleteAssetIDs(assets []store.MediaDeleteAsset) []int64 {
	ids := make([]int64, 0, len(assets))
	for _, asset := range assets {
		ids = append(ids, asset.ID)
	}
	return ids
}

// mediaDeleteCandidateIDs は失敗メッセージ用に、戻し候補の ID を並べる。
func mediaDeleteCandidateIDs(assets []store.MediaDeleteRevertCandidate) []int64 {
	ids := make([]int64, 0, len(assets))
	for _, asset := range assets {
		ids = append(ids, asset.ID)
	}
	return ids
}
