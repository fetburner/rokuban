package contracttest

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

func runUntilEncodedPresence(t *testing.T, newStore NewMediaDeleteStore) {
	for _, tc := range []struct {
		kind string
		read func(store.MediaServing, context.Context, int64) (store.MediaAsset, error)
	}{
		{"encoded", func(serving store.MediaServing, ctx context.Context, recordingID int64) (store.MediaAsset, error) {
			return serving.GetEncodedMediaAssetForServing(ctx, recordingID, "h264")
		}},
		{"thumbnail", store.MediaServing.GetThumbnailMediaAssetForServing},
		{"seek_tiles", store.MediaServing.GetSeekTilesMediaAssetForServing},
	} {
		t.Run("missing "+tc.kind, func(t *testing.T) {
			f := newStore(t)
			ctx := t.Context()
			_, present := f.NewUntilEncodedOriginal(t)
			missingRecording, missing := f.NewUntilEncodedOriginal(t)
			asset, err := tc.read(f.Serving, ctx, missingRecording)
			if err != nil {
				t.Fatalf("getting %s before reporting it missing: %v", tc.kind, err)
			}
			if err := f.Storage.UpsertMissingMediaAsset(ctx, f.CopyID(t, asset.ID)); err != nil {
				t.Fatalf("reporting missing %s: %v", tc.kind, err)
			}
			rows, err := f.Store.ListUntilEncodedOriginalsToDelete(ctx, 10)
			if err != nil || len(rows) != 1 || rows[0].ID != f.CopyID(t, present) {
				t.Errorf("until-encoded candidates = %+v, %v; want original %d eligible and %d kept", rows, err, present, missing)
			}
		})
	}
}

func runOldGenerationPresence(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	present, _ := f.NewOldGeneration(t)
	missing, missingCurrent := f.NewOldGeneration(t)
	if err := f.Storage.UpsertMissingMediaAsset(t.Context(), missingCurrent); err != nil {
		t.Fatalf("reporting missing current generation: %v", err)
	}
	rows, err := f.Store.ListOldGenerationMediaAssetCopiesToDelete(t.Context(), time.Now().Add(time.Hour), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != present {
		t.Errorf("old-generation candidates = %+v, %v; want copy %d eligible and %d kept", rows, err, present, missing)
	}
}

func runRemovedEncodedPresence(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name, originalState                     string
		keepEncoded, removedMissing, wantListed bool
	}{
		// 対照: 外した版も残る版も実体有り。
		{"both versions present", "active", true, false, true},
		// 外した版自身の実体が無くても、残る版があれば列挙される。
		{"removed version missing", "active", true, true, true},
		// 残る版が実体無しの原本だけなら、最後の実体を消さない。
		{"only a missing original remains", "missing", false, false, false},
		// 外した版以外に原本の active コピーも残す encoded も無い。
		{"no remaining version", "deleted", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordingID, assetID := f.NewRemovedEncoded(t)
			copyID := f.CopyID(t, assetID)
			if tc.keepEncoded {
				f.NewRetainedEncoded(t, recordingID)
			}
			original, err := f.Serving.GetOriginalMediaAssetForServing(ctx, recordingID)
			if err != nil {
				t.Fatalf("getting original before changing presence: %v", err)
			}
			switch tc.originalState {
			case "missing":
				if err := f.Storage.UpsertMissingMediaAsset(ctx, f.CopyID(t, original.ID)); err != nil {
					t.Fatalf("reporting missing original: %v", err)
				}
			case "deleted":
				f.SetAssetState(t, original.ID, "deleted")
			}
			if tc.removedMissing {
				if err := f.Storage.UpsertMissingMediaAsset(ctx, copyID); err != nil {
					t.Fatalf("reporting removed version missing: %v", err)
				}
			}
			rows, err := f.Store.ListRemovedEncodedAssetsToDelete(ctx, 100)
			if err != nil || containsMediaDeleteAsset(rows, copyID) != tc.wantListed {
				t.Errorf("removed encoded candidates = %+v, %v; want copy %d listed=%v", rows, err, copyID, tc.wantListed)
			}
		})
	}
}
