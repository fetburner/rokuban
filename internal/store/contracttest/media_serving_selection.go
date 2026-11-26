package contracttest

import (
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// runServingCopySelection は配信の読み手がコピーを選ぶ規則を確かめる。
// 置き場で絞らない読み手は、active なコピーが複数あれば置き場名順で先頭の 1 件を選ぶ。deleting のコピーは候補に入らない。
// 置き場を設定した読み手は、その置き場のコピーを選ぶ。
// 資産は tokyo（fixture の既定の置き場）・a-store（deleting）・b-store・z-store のコピーを持つ。
func runServingCopySelection(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	rec := f.NewRecording(t)
	profile := "h264"
	type reader struct {
		name string
		kind string
		read func(store.MediaServing) (store.MediaAsset, error)
	}
	readers := []reader{
		{"original", store.AssetKindOriginal, func(s store.MediaServing) (store.MediaAsset, error) {
			return s.GetOriginalMediaAssetForServing(ctx, rec)
		}},
		{"active origin", store.AssetKindOriginal, func(s store.MediaServing) (store.MediaAsset, error) {
			return s.GetActiveOriginalMediaAsset(ctx, rec)
		}},
		{"encoded", store.AssetKindEncoded, func(s store.MediaServing) (store.MediaAsset, error) {
			return s.GetEncodedMediaAssetForServing(ctx, rec, profile)
		}},
		{"thumbnail", store.AssetKindThumbnail, func(s store.MediaServing) (store.MediaAsset, error) {
			return s.GetThumbnailMediaAssetForServing(ctx, rec)
		}},
		{"seek tiles", store.AssetKindSeekTiles, func(s store.MediaServing) (store.MediaAsset, error) {
			return s.GetSeekTilesMediaAssetForServing(ctx, rec)
		}},
	}
	for _, kind := range []string{store.AssetKindOriginal, store.AssetKindEncoded, store.AssetKindThumbnail, store.AssetKindSeekTiles} {
		var p *string
		if kind == store.AssetKindEncoded {
			p = &profile
		}
		assetID := f.NewAsset(t, rec, kind, p, fmt.Sprintf("sites/%s/serving-selection/%s", store.DefaultSite, kind), "active")
		deleting := f.AddCopy(t, assetID, "a-store", fmt.Sprintf("a/%s", kind))
		f.SetCopyState(t, deleting, "deleting")
		f.AddCopy(t, assetID, "b-store", fmt.Sprintf("b/%s", kind))
		f.AddCopy(t, assetID, "z-store", fmt.Sprintf("z/%s", kind))
	}
	for _, r := range readers {
		got, err := r.read(f.ServingIn(""))
		if err != nil {
			t.Errorf("%s without a configured store: %v", r.name, err)
		} else if got.Store != "b-store" {
			t.Errorf("%s without a configured store picked %q, want b-store (first active copy by store name; a-store is deleting)", r.name, got.Store)
		}
		got, err = r.read(f.ServingIn("z-store"))
		if err != nil || got.Store != "z-store" {
			t.Errorf("%s with z-store = (%+v, %v), want the z-store copy", r.name, got, err)
		}
	}
}
