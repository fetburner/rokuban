package contracttest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// RunMediaReconcileCalls は、reconcile が呼ぶ読み取りのうち、契約に主張が無かったものを検査する。
// 本番の呼び出し元（worker の reconcile・起動時の検査）が頼る形だけを主張にする。
func RunMediaReconcileCalls(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("unsatisfiable profiles count recordings whose desired profile is not known", func(t *testing.T) {
		runUnsatisfiableProfiles(t, newFixture(t))
	})
	t.Run("encoded meaning evidence lists active encoded paths and rebuild requests", func(t *testing.T) {
		runEncodedMeaningEvidence(t, newFixture(t))
	})
	t.Run("missing thumbnail and seek tile recordings need a present original and no active derived copy", func(t *testing.T) {
		runMissingDerivedReconcile(t, newFixture(t))
	})
	t.Run("derived generation is reported only for recordings with the kind", func(t *testing.T) {
		runDerivedGenerationPresence(t, newFixture(t))
	})
}

func runUnsatisfiableProfiles(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil, "originals/unsatisfiable.ts", "active")
	f.SetEncodeProfiles(t, id, []string{"h264"})
	got, err := f.Reconcile.ListUnsatisfiableEncodeProfiles(ctx, []string{"other"})
	if err != nil || len(got) != 1 || got[0].Profile != "h264" || got[0].Recordings != 1 {
		t.Fatalf("ListUnsatisfiableEncodeProfiles(known=[other]) = (%+v, %v), want [h264: 1 recording]", got, err)
	}
	got, err = f.Reconcile.ListUnsatisfiableEncodeProfiles(ctx, []string{"h264"})
	if err != nil || len(got) != 0 {
		t.Fatalf("ListUnsatisfiableEncodeProfiles(known=[h264]) = (%+v, %v), want none", got, err)
	}
}

func runEncodedMeaningEvidence(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	id := f.NewRecording(t)
	mobile := "mobile"
	path := fmt.Sprintf("sites/%s/%d/mobile.mp4", f.Site, id)
	f.NewAsset(t, id, "encoded", &mobile, path, "active")
	f.NewAsset(t, id, "original", nil, "originals/evidence.ts", "active")
	f.SetEncodedAssetRebuildRequest(t, id, "cut", []chapters.Range{{StartMs: 0, EndMs: 1000}})
	got, err := f.Reconcile.ListEncodedMeaningEvidence(ctx)
	if err != nil {
		t.Fatalf("ListEncodedMeaningEvidence: %v", err)
	}
	want := []store.EncodedMeaningEvidence{{Profile: "cut", RelPath: ""}, {Profile: "mobile", RelPath: path}}
	if !sameMeaningEvidence(got, want) {
		t.Fatalf("ListEncodedMeaningEvidence = %+v, want %+v", got, want)
	}
}

func sameMeaningEvidence(got, want []store.EncodedMeaningEvidence) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}

// runMissingDerivedReconcile は次の 3 録画を使う。
// A は実体のある原本だけ。B は原本と thumbnail。C は原本の実体が無い（missing）。
// サムネイルの候補は A だけ、シークタイルの候補は A と B。
func runMissingDerivedReconcile(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	a, b, c := f.NewRecording(t), f.NewRecording(t), f.NewRecording(t)
	f.NewAsset(t, a, "original", nil, "originals/a.ts", "active")
	f.NewAsset(t, b, "original", nil, "originals/b.ts", "active")
	f.NewAsset(t, b, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", b), "active")
	cOriginal := f.NewAsset(t, c, "original", nil, "originals/c.ts", "active")
	f.SetCopyMissing(t, f.CopyID(t, cOriginal), true)
	page := store.MediaReconcilePage{RowLimit: 10}

	thumbs, err := f.Reconcile.ListMissingThumbnailRecordings(ctx, page)
	if err != nil || !slices.Equal(thumbs, []int64{a}) {
		t.Fatalf("ListMissingThumbnailRecordings = (%v, %v), want only recording %d", thumbs, err, a)
	}
	f.NewAsset(t, b, "seek_tiles", nil, fmt.Sprintf("thumbnails/%d_tiles.jpg", b), "active")
	tiles, err := f.Reconcile.ListMissingSeekTilesRecordings(ctx, page)
	if err != nil || !slices.Equal(tiles, []int64{a}) {
		t.Fatalf("ListMissingSeekTilesRecordings after seek tiles of %d = (%v, %v), want only recording %d", b, tiles, err, a)
	}
}

func runDerivedGenerationPresence(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	withThumbnail, without := f.NewRecording(t), f.NewRecording(t)
	f.NewAsset(t, withThumbnail, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", withThumbnail), "active")
	generation, found, err := f.Reconcile.GetDerivedMediaAssetGeneration(ctx, withThumbnail, "thumbnail")
	if err != nil || !found || generation != 0 {
		t.Fatalf("GetDerivedMediaAssetGeneration(thumbnail) = (%d, %t, %v), want (0, true, nil)", generation, found, err)
	}
	if _, found, err := f.Reconcile.GetDerivedMediaAssetGeneration(ctx, without, "thumbnail"); err != nil || found {
		t.Fatalf("GetDerivedMediaAssetGeneration without a thumbnail = (found=%t, %v), want not found", found, err)
	}
}
