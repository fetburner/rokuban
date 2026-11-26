package contracttest

import (
	"context"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// InPlaceRegistrar は既存ファイルを指す録画とアセットを登録する store である。
type InPlaceRegistrar interface {
	RegisterInPlace(ctx context.Context, input store.InPlaceInput) (store.InPlaceResult, error)
}

// runTSScanInPlaceInvalidation は、原本の in-place 登録が内容を変えたときだけ計測を捨てることを検査する。
func runTSScanInPlaceInvalidation(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	input := f.NewInPlaceOriginalInput(t, "original/first.ts")
	result, err := f.InPlace.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	id := result.AssetIDs[0]
	source, err := f.Store.GetActiveOriginalForTSScan(ctx, result.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := f.Store.ReplaceScan(ctx, source, 188, nil, nil); err != nil || !applied {
		t.Fatalf("initial ReplaceScan = %v, %v, want applied", applied, err)
	}
	assertCurrentTSScanMarker(t, f, result.RecordingID, 188)
	if _, err := f.InPlace.RegisterInPlace(ctx, input); err != nil {
		t.Fatal(err)
	}
	assertCurrentTSScanMarker(t, f, result.RecordingID, 188) // 同じ active パスの再登録は no-op。
	input.Assets[0].RelPath = "original/blocked.ts"
	if _, err := f.InPlace.RegisterInPlace(ctx, input); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("registration with live copy = %v, want ErrConflict", err)
	}
	assertCurrentTSScanMarker(t, f, result.RecordingID, 188) // 拒否された登録は計測を捨てない。
	f.SetAssetSHA256(t, id)
	assertCurrentTSScanMarker(t, f, result.RecordingID, 188) // 内容のリビジョンとサイズに無い列は計測を捨てない。
	f.SetAssetState(t, id, "deleted")
	input.Assets[0].RelPath = "original/replacement.ts"
	replaced, err := f.InPlace.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.AssetIDs[0] != id {
		t.Fatal("replacement must update the existing logical asset")
	}
	if got := readTSScanSnapshot(t, f, result.RecordingID).ScannedSizeBytes; got != -1 {
		t.Fatalf("scan marker after replacement = %d, want none", got)
	}
}

// runTSScanInPlaceReplacementInFlight は、読み出しの後に in-place 登録で内容が変わった原本への置き換えが書かれないことを検査する。
func runTSScanInPlaceReplacementInFlight(t *testing.T, newStore NewTSScanStore) {
	for _, otherStore := range []bool{false, true} {
		name := "same_store"
		if otherStore {
			name = "other_store"
		}
		t.Run(name, func(t *testing.T) {
			f := newStore(t)
			ctx := context.Background()
			input := f.NewInPlaceOriginalInput(t, "original/first.ts")
			result, err := f.InPlace.RegisterInPlace(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			source, err := f.Store.GetActiveOriginalForTSScan(ctx, result.RecordingID)
			if err != nil {
				t.Fatal(err)
			}
			writer := f.InPlace
			if otherStore {
				writer = f.OtherStoreInPlace
			} else {
				f.SetAssetState(t, source.ID, "deleted")
			}
			input.Assets[0].RelPath = "original/replacement.ts"
			replaced, err := writer.RegisterInPlace(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if replaced.AssetIDs[0] != source.ID {
				t.Fatal("replacement did not update original")
			}
			applied, err := f.Store.ReplaceScan(ctx, source, 188, []store.RecordingDropStat{{Pid: 256, Packets: 1}}, nil)
			if err != nil || applied {
				t.Fatalf("stale ReplaceScan = %v, %v, want false, nil", applied, err)
			}
			if got := readTSScanSnapshot(t, f, result.RecordingID); len(got.Stats) != 0 || got.ScannedSizeBytes != -1 {
				t.Fatalf("stale scan wrote %d stats and marker %d, want none", len(got.Stats), got.ScannedSizeBytes)
			}
			fresh, err := f.Store.GetActiveOriginalForTSScan(ctx, result.RecordingID)
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := f.Store.ReplaceScan(ctx, fresh, 188, nil, nil); err != nil || !applied {
				t.Fatalf("fresh ReplaceScan = %v, %v, want applied", applied, err)
			}
			assertCurrentTSScanMarker(t, f, result.RecordingID, 188)
		})
	}
}

// runTSScanCatalogRestoreInFlight は、読み出しの後に catalog の復元で内容のリビジョンが進んだ原本への置き換えが書かれないことを検査する。
func runTSScanCatalogRestoreInFlight(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID, _ := f.NewOriginal(t, 6, false)
	source, err := f.Store.GetActiveOriginalForTSScan(ctx, recordingID)
	if err != nil {
		t.Fatal(err)
	}
	document, err := f.Catalog.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Catalog.Restore(ctx, document, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	applied, err := f.Store.ReplaceScan(ctx, source, 6, []store.RecordingDropStat{{Pid: 256, Packets: 1}}, nil)
	if err != nil || applied {
		t.Fatalf("ReplaceScan after catalog restore = %v, %v, want false, nil", applied, err)
	}
	if got := readTSScanSnapshot(t, f, recordingID); len(got.Stats) != 0 || got.ScannedSizeBytes != -1 {
		t.Fatalf("scan after catalog restore wrote %d stats and marker %d, want none", len(got.Stats), got.ScannedSizeBytes)
	}
	fresh, err := f.Store.GetActiveOriginalForTSScan(ctx, recordingID)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := f.Store.ReplaceScan(ctx, fresh, 6, nil, nil); err != nil || !applied {
		t.Fatalf("fresh ReplaceScan after catalog restore = %v, %v, want applied", applied, err)
	}
}
