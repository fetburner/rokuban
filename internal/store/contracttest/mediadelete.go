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
type MediaDeleteFixture struct {
	Store                         store.MediaDelete
	Serving                       store.MediaServing
	Catalog                       store.Catalog
	NewUntilEncodedOriginal       func(t *testing.T) (recordingID, assetID int64)
	BeginUntilEncodedInvalidation func(t *testing.T, recordingID int64) (commit func())
	NewDeletedAsset               func(t *testing.T) (recordingID, assetID int64)
	NewRemovedEncoded             func(t *testing.T) (recordingID, assetID int64)
	// RemovalRequestExists is retained because ListRemovedEncodedAssetsToDelete intentionally
	// omits deleted assets, so it cannot reveal whether MarkDeleted cleared their request row.
	RemovalRequestExists func(t *testing.T, recordingID int64, profile string) bool
	WaitForLockWaiter    func(t *testing.T, operation string)
}

// NewMediaDeleteStore はメディア削除契約の実装とフィクスチャを作る。
type NewMediaDeleteStore func(t *testing.T) MediaDeleteFixture

// RunMediaDelete はメディア削除の契約を実行する。
func RunMediaDelete(t *testing.T, newStore NewMediaDeleteStore) {
	t.Helper()
	t.Run("until-encoded transition waits for and rechecks recording writes", func(t *testing.T) {
		runUntilEncodedSerialization(t, newStore)
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
	removed, err := f.Store.ListRemovedEncodedAssetsToDelete(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRemovedEncodedAssetsToDelete(before delete): %v", err)
	}
	if !containsMediaDeleteAsset(removed, assetID) {
		t.Fatalf("removed encoded assets = %+v, want asset %d", removed, assetID)
	}
	if err := f.Store.MarkDeleting(context.Background(), assetID); err != nil {
		t.Fatalf("MarkDeleting(%d): %v", assetID, err)
	}
	if err := f.Store.MarkDeleted(context.Background(), assetID); err != nil {
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
	candidates, err := f.Store.ListUntilEncodedOriginalsToDelete(ctx, 10)
	if err != nil {
		t.Fatalf("listing eligible until_encoded originals: %v", err)
	}
	if !containsMediaDeleteAsset(candidates, assetID) {
		t.Fatalf("until_encoded candidates = %+v, want asset %d", candidates, assetID)
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
		marked, err := f.Store.MarkUntilEncodedDeleting(ctx, recordingID, assetID)
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
		t.Fatalf("active original after eligibility changed = (%+v, %v), want asset %d", active, err, assetID)
	}
}

func runDeletedAssetIsNotReverted(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	recordingID, assetID := f.NewDeletedAsset(t)
	updated, err := f.Store.RevertToActive(context.Background(), assetID, time.Now())
	if err != nil {
		t.Fatalf("RevertToActive(deleted) error = %v", err)
	}
	if updated {
		t.Fatal("RevertToActive(deleted) updated the asset, want no change")
	}
	assertMediaAssetDeleted(t, f, recordingID, assetID, store.AssetKindOriginal, "")
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
	if deletedAsset == nil || deletedAsset.RecordingID != recordingID || deletedAsset.State != "deleted" || deletedAsset.DeletedAt == nil {
		t.Fatalf("Catalog.Export.MediaAssets RecordingID/State/DeletedAt for asset %d = %+v, want recording %d in deleted state with deleted_at", assetID, deletedAsset, recordingID)
	}
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
	pending, err := f.Store.ListPendingDeleteAssets(context.Background(), cutoff, 100)
	if err != nil {
		t.Fatalf("ListPendingDeleteAssets when checking deleted asset %d: %v", assetID, err)
	}
	unqualified, err := f.Store.ListUnqualifiedDeletingAssets(context.Background(), cutoff, 100)
	if err != nil {
		t.Fatalf("ListUnqualifiedDeletingAssets when checking deleted asset %d: %v", assetID, err)
	}
	if containsMediaDeleteAsset(pending, assetID) || containsMediaDeleteCandidate(unqualified, assetID) {
		t.Fatalf("deleted asset %d remains in a deleting queue: pending=%+v unqualified=%+v", assetID, pending, unqualified)
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
