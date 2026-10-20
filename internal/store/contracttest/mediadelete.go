package contracttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// MediaDeleteFixture はメディア削除契約と、その状態を作るための実装別フックをまとめる。
type MediaDeleteFixture struct {
	Store                         store.MediaDelete
	NewUntilEncodedOriginal       func(t *testing.T) (recordingID, assetID int64)
	BeginUntilEncodedInvalidation func(t *testing.T, recordingID int64) (commit func())
	NewDeletedAsset               func(t *testing.T) int64
	ReadAssetState                func(t *testing.T, assetID int64) string
	WaitForLockWaiter             func(t *testing.T, operation string)
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
	if state := f.ReadAssetState(t, assetID); state != "active" {
		t.Fatalf("asset state = %q, want active after eligibility changed", state)
	}
}

func runDeletedAssetIsNotReverted(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	assetID := f.NewDeletedAsset(t)
	updated, err := f.Store.RevertToActive(context.Background(), assetID, time.Now())
	if err != nil {
		t.Fatalf("RevertToActive(deleted) error = %v", err)
	}
	if updated {
		t.Fatal("RevertToActive(deleted) updated the asset, want no change")
	}
	if state := f.ReadAssetState(t, assetID); state != "deleted" {
		t.Fatalf("asset state = %q, want deleted", state)
	}
}

func containsMediaDeleteAsset(assets []store.MediaDeleteAsset, id int64) bool {
	for _, asset := range assets {
		if asset.ID == id {
			return true
		}
	}
	return false
}
