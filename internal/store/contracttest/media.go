// Package contracttest は store の契約を実装に対して検査する統合テストを持つ。
package contracttest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// MediaFixture はメディア契約の実装と状態を作るフックをまとめる。
type MediaFixture struct {
	Serving   store.MediaServing
	Reconcile store.MediaReconcile
	Storage   store.StorageObservation

	NewRecording  func(t *testing.T) int64
	NewAsset      func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64
	SetAssetState func(t *testing.T, assetID int64, state string)
}

// NewMediaFixture は実在の契約実装とフィクスチャを作る。
type NewMediaFixture func(t *testing.T) MediaFixture

// RunMedia は実在のメディア store 実装に対して配信・名前空間・監査の契約を実行する。
func RunMedia(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
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
