package contracttest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// runMediaReconcileCalls は、reconcile が呼ぶ読み取りのうち、どの契約からも主張されていなかったものを検査する。
// 既存の主張（RunMedia と RunMediaPublish の他の行）と重ならない形だけを置く。
func runMediaReconcileCalls(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("unsatisfiable profiles count recordings whose desired profile is not known", func(t *testing.T) {
		runUnsatisfiableProfiles(t, newFixture(t))
	})
	t.Run("encoded meaning evidence lists active encoded paths and rebuild requests", func(t *testing.T) {
		runEncodedMeaningEvidence(t, newFixture(t))
	})
	t.Run("derived generation is the highest used copy generation, including reclaimed copies", func(t *testing.T) {
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

// runDerivedGenerationPresence は、サムネイルの世代を返す主張を 1 行にまとめる。
// 回収済みのコピーが派生の世代より大きい世代を使っていれば、その世代を返す。
// 呼び出し側は返り値に 1 を足した世代を次の試行に使うので、回収済みのキーを再利用しない。
func runDerivedGenerationPresence(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	withThumbnail, without, reclaimed := f.NewRecording(t), f.NewRecording(t), f.NewRecording(t)
	f.NewAsset(t, withThumbnail, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", withThumbnail), "active")
	thumbnail := f.NewAsset(t, reclaimed, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", reclaimed), "active")
	f.AddTombstoneCopy(t, thumbnail, "tokyo", fmt.Sprintf("thumbnails/%d-old.jpg", reclaimed), 3)
	for _, tc := range []struct {
		name       string
		id         int64
		generation int64
		found      bool
	}{
		{"active thumbnail", withThumbnail, 0, true},
		{"no thumbnail", without, 0, false},
		{"reclaimed copy above the derived generation", reclaimed, 3, true},
	} {
		generation, found, err := f.Reconcile.GetDerivedMediaAssetGeneration(ctx, tc.id, "thumbnail")
		if err != nil || found != tc.found || (found && generation != tc.generation) {
			t.Errorf("GetDerivedMediaAssetGeneration (%s) = (%d, %t, %v), want (%d, %t, nil)", tc.name, generation, found, err, tc.generation, tc.found)
		}
	}
}
