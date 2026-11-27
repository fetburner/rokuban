package contracttest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// runServingExcludesTrashed は、ごみ箱の録画の原本と encoded を配信の読み手が返さないことを検査する。
// 各ケースは、ごみ箱に入れる前に同じ読み取りが成功することを先に確かめる。そうしないと、
// アセットの作成が失敗していても ErrNotFound で通ってしまう。
func runServingExcludesTrashed(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	profile := "h264"
	cases := []struct {
		name    string
		kind    string
		profile *string
		get     func(id int64) (store.MediaAsset, error)
	}{
		{"original without a store", "original", nil,
			func(id int64) (store.MediaAsset, error) {
				return f.ServingIn("").GetOriginalMediaAssetForServing(ctx, id)
			}},
		{"original through a store-bound reader", "original", nil,
			func(id int64) (store.MediaAsset, error) { return f.Serving.GetOriginalMediaAssetForServing(ctx, id) }},
		{"encoded without a store", "encoded", &profile,
			func(id int64) (store.MediaAsset, error) {
				return f.ServingIn("").GetEncodedMediaAssetForServing(ctx, id, profile)
			}},
		{"encoded through a store-bound reader", "encoded", &profile,
			func(id int64) (store.MediaAsset, error) {
				return f.ServingIn("tokyo").GetEncodedMediaAssetForServing(ctx, id, profile)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := f.NewRecording(t)
			path := fmt.Sprintf("sites/home/%d/%s.bin", id, tc.kind)
			assetID := f.NewAsset(t, id, tc.kind, tc.profile, path, "active")
			got, err := tc.get(id)
			if err != nil || got.ID != assetID || got.RelPath != path {
				t.Fatalf("before trashing: %+v, %v; want asset %d at %q", got, err, assetID, path)
			}
			f.SetRecordingTrashed(t, id)
			if got, err := tc.get(id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("after trashing: %+v, %v; want ErrNotFound", got, err)
			}
		})
	}
}
