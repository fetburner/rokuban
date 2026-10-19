// Package contracttest は store の契約を実装に対して検査する統合テストを持つ。
package contracttest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// MediaFixture はメディア契約の実装と状態を作るフックをまとめる。
type MediaFixture struct {
	Serving   store.MediaServing
	Reconcile store.MediaReconcile
	Storage   store.StorageObservation

	NewRecording                  func(t *testing.T) int64
	NewAsset                      func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64
	SetAssetState                 func(t *testing.T, assetID int64, state string)
	SetMediaAssetKeepRanges       func(t *testing.T, assetID int64, keep []chapters.Range)
	SetEncodedAssetRebuildRequest func(t *testing.T, recordingID int64, profile string, keep []chapters.Range)
}

// NewMediaFixture は実在の契約実装とフィクスチャを作る。
type NewMediaFixture func(t *testing.T) MediaFixture

// RunMedia は実在のメディア store 実装に対して配信・名前空間・監査の契約を実行する。
func RunMedia(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("media jobs read active inputs through their contract", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.NewRecording(t)
		if _, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetActiveOriginalMediaAsset(%d) error = %v, want ErrNotFound", id, err)
		}
		if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
			t.Fatalf("HasActiveThumbnailMediaAsset(%d) = (%v, %v), want (false, nil)", id, has, err)
		}

		originalPath := fmt.Sprintf("sites/home/%d/original.m2ts", id)
		originalID := f.NewAsset(t, id, "original", nil, originalPath, "active")
		original, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id)
		if err != nil {
			t.Fatalf("GetActiveOriginalMediaAsset(%d): %v", id, err)
		}
		if original.ID != originalID || original.RelPath != originalPath {
			t.Fatalf("active original = %+v, want id %d and path %q", original, originalID, originalPath)
		}

		thumbnailID := f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
		if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || !has {
			t.Fatalf("HasActiveThumbnailMediaAsset(%d) = (%v, %v), want (true, nil)", id, has, err)
		}
		f.SetAssetState(t, thumbnailID, "deleted")
		if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
			t.Fatalf("HasActiveThumbnailMediaAsset(%d) after deletion = (%v, %v), want (false, nil)", id, has, err)
		}
		f.SetAssetState(t, originalID, "deleted")
		if _, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetActiveOriginalMediaAsset(%d) after deletion error = %v, want ErrNotFound", id, err)
		}
	})

	t.Run("encode planning reads asset and request state through its contract", func(t *testing.T) {
		f := newFixture(t)
		runMediaEncodePlanningContract(t, f)
	})

	t.Run("serving returns only active assets", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		for _, state := range []string{"deleting", "deleted"} {
			t.Run("original "+state, func(t *testing.T) {
				id := f.NewRecording(t)
				f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/home/%d/original.m2ts", id), state)
				if _, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("GetOriginalMediaAssetForServing(%d) error = %v, want ErrNotFound", id, err)
				}
			})
			t.Run("encoded "+state, func(t *testing.T) {
				id := f.NewRecording(t)
				profile := "h264"
				f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/encoded.mp4", id), state)
				if _, err := f.Serving.GetEncodedMediaAssetForServing(ctx, id, profile); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("GetEncodedMediaAssetForServing(%d, %q) error = %v, want ErrNotFound", id, profile, err)
				}
			})
		}

		id := f.NewRecording(t)
		path := "sites/home/active.m2ts"
		assetID := f.NewAsset(t, id, "original", nil, path, "active")
		asset, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id)
		if err != nil {
			t.Fatalf("GetOriginalMediaAssetForServing(%d): %v", id, err)
		}
		if asset.ID != assetID || asset.RelPath != path {
			t.Fatalf("served original = %+v, want id %d and path %q", asset, assetID, path)
		}
	})

	t.Run("namespace guard accepts site paths and rejects malformed ones", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		validID := f.NewRecording(t)
		f.NewAsset(t, validID, "original", nil, "sites/home/archive/show.m2ts", "active")
		if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx); err != nil || violation != nil {
			t.Fatalf("FindInvalidMediaAssetRelPath() = (%+v, %v), want (nil, nil)", violation, err)
		}

		invalidID := f.NewRecording(t)
		assetID := f.NewAsset(t, invalidID, "original", nil, "sites//archive/show.m2ts", "active")
		violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx)
		if err != nil {
			t.Fatalf("FindInvalidMediaAssetRelPath(): %v", err)
		}
		if violation == nil || violation.ID != assetID || violation.RelPath != "sites//archive/show.m2ts" {
			t.Fatalf("violation = %+v, want asset %d with malformed site path", violation, assetID)
		}
		f.SetAssetState(t, assetID, "deleted")
		if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx); err != nil || violation != nil {
			t.Fatalf("FindInvalidMediaAssetRelPath() after deleting malformed row = (%+v, %v), want (nil, nil)", violation, err)
		}
	})

	t.Run("audit keeps the full site-prefixed asset path", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		const relPath = "sites/home/archive/20240101/show.m2ts"
		id := f.NewRecording(t)
		f.NewAsset(t, id, "original", nil, relPath, "deleted")
		paths, err := f.Storage.ListAllMediaAssetRelPaths(ctx)
		if err != nil {
			t.Fatalf("ListAllMediaAssetRelPaths(): %v", err)
		}
		for _, got := range paths {
			if got == relPath {
				return
			}
		}
		t.Fatalf("ListAllMediaAssetRelPaths() = %q, want to retain %q", paths, relPath)
	})
}

func runMediaEncodePlanningContract(t *testing.T, f MediaFixture) {
	t.Helper()
	ctx := context.Background()
	id := f.NewRecording(t)
	profile := "cut"
	assetID := f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/cut.mp4", id), "deleted")
	asset, err := f.Reconcile.GetEncodedAssetForPlan(ctx, id, profile)
	if err != nil {
		t.Fatalf("GetEncodedAssetForPlan(%d, %q): %v", id, profile, err)
	}
	if asset.ID != assetID || asset.State != "deleted" || asset.RelPath != fmt.Sprintf("sites/home/%d/cut.mp4", id) {
		t.Fatalf("encoded asset observation = %+v, want id %d with deleted state and its path", asset, assetID)
	}
	if keep, err := f.Reconcile.GetMediaAssetKeepRanges(ctx, assetID); err != nil || len(keep) != 0 {
		t.Fatalf("GetMediaAssetKeepRanges(%d) before freezing = (%v, %v), want ([], nil)", assetID, keep, err)
	}

	f.SetAssetState(t, assetID, "active")
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	f.SetMediaAssetKeepRanges(t, assetID, keep)
	gotKeep, err := f.Reconcile.GetMediaAssetKeepRanges(ctx, assetID)
	if err != nil {
		t.Fatalf("GetMediaAssetKeepRanges(%d): %v", assetID, err)
	}
	if !chapters.SameRanges(gotKeep, keep) {
		t.Fatalf("frozen keep ranges = %v, want %v", gotKeep, keep)
	}

	matches, err := f.Reconcile.EncodedAssetRebuildRequestMatches(ctx, id, profile, keep)
	if err != nil || matches {
		t.Fatalf("EncodedAssetRebuildRequestMatches without a request = (%v, %v), want (false, nil)", matches, err)
	}
	f.SetEncodedAssetRebuildRequest(t, id, profile, keep)
	if matches, err := f.Reconcile.EncodedAssetRebuildRequestMatches(ctx, id, profile, keep); err != nil || !matches {
		t.Fatalf("EncodedAssetRebuildRequestMatches matching keep = (%v, %v), want (true, nil)", matches, err)
	}
	changedKeep := []chapters.Range{{StartMs: 0, EndMs: 900}}
	if matches, err := f.Reconcile.EncodedAssetRebuildRequestMatches(ctx, id, profile, changedKeep); err != nil || matches {
		t.Fatalf("EncodedAssetRebuildRequestMatches changed keep = (%v, %v), want (false, nil)", matches, err)
	}
}
