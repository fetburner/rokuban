package contracttest

import (
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

func runTransferSourcePresence(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	present := f.NewAsset(t, f.NewRecording(t), "original", nil, "originals/present.ts", "active")
	missing := f.NewAsset(t, f.NewRecording(t), "original", nil, "originals/missing.ts", "active")
	observeMissingContractCopy(t, f, missing)

	// 新しいコピーが実体無しなら、実体有りの古いコピーを転送元に選ぶ。
	presentCopy := f.CopyID(t, present)
	newerCopy := f.AddCopy(t, present, "osaka", "originals/newer.ts")
	now := time.Now()
	f.SetCopyCreatedAt(t, presentCopy, now.Add(-time.Hour))
	f.SetCopyCreatedAt(t, newerCopy, now)
	if err := f.OpenStore("osaka").Storage.UpsertMissingMediaAsset(ctx, newerCopy); err != nil {
		t.Fatalf("marking newest source missing: %v", err)
	}
	fs := []string{"tokyo", "osaka", "takamatsu"}
	policy := store.NewStoragePolicy([]store.OriginalStoreRule{{Store: "takamatsu"}}, fs, "remote")
	transfer := f.OpenTransfer("tokyo", policy)
	rows, err := transfer.ListMediaAssetTransferCandidates(ctx, 10, 100)
	want := []store.MediaAssetTransferCandidate{{MediaAssetID: present, Store: "takamatsu", SourceStore: "tokyo"}}
	if err != nil || !slices.Equal(rows, want) {
		t.Fatalf("transfer candidates = %+v, %v; want %+v", rows, err, want)
	}
	got, ok, err := transfer.PrepareMediaAssetTransfer(ctx, present, "takamatsu", fs, fs, nil, 10)
	if err != nil || !ok || got.SourceStore != "tokyo" || got.SourceCopyID != presentCopy {
		t.Errorf("prepared present source = %+v, %v, %v; want Tokyo copy %d", got, ok, err, presentCopy)
	}
	if got, ok, err := transfer.PrepareMediaAssetTransfer(ctx, missing, "takamatsu", fs, fs, nil, 10); err != nil || ok {
		t.Errorf("prepared missing-only source = %+v, %v, %v; want skipped", got, ok, err)
	}
}

func runPlacementPruningPresence(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	present := f.NewAsset(t, f.NewRecording(t), "thumbnail", nil, "thumbnails/present.jpg", "active")
	missing := f.NewAsset(t, f.NewRecording(t), "thumbnail", nil, "thumbnails/missing.jpg", "active")
	f.AddCopy(t, present, "osaka", "thumbnails/present.jpg")
	missingCopy := f.AddCopy(t, missing, "osaka", "thumbnails/missing.jpg")
	if err := f.OpenStore("osaka").Storage.UpsertMissingMediaAsset(ctx, missingCopy); err != nil {
		t.Fatalf("marking desired copy missing: %v", err)
	}
	policy := store.NewStoragePolicy(nil, []string{"tokyo", "osaka"}, "osaka").WithPlacementEnabled(true)
	transfer := f.OpenTransfer("tokyo", policy)
	rows, err := transfer.ListMediaAssetCopiesToDelete(ctx, time.Now().Add(time.Hour), 10)
	if err != nil || len(rows) != 1 || rows[0].MediaAssetID != present || rows[0].CopyID != f.CopyID(t, present) {
		t.Errorf("placement deletion candidates = %+v, %v; want only retiring copy of asset %d", rows, err, present)
	}
}

func runCorruptCopyRetirementPresence(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	transfer := f.OpenTransfer("tokyo", store.StoragePolicy{})
	for _, tc := range []struct {
		name            string
		originalMissing bool
		wantMarked      bool
		wantState       string
	}{
		{"present original allows retiring the derived copy", false, true, "deleting"},
		{"missing original keeps the derived copy active", true, false, "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := f.NewRecording(t)
			original := f.NewAsset(t, id, "original", nil, "originals/"+tc.wantState+".ts", "active")
			if tc.originalMissing {
				observeMissingContractCopy(t, f, original)
			}
			asset := f.NewAsset(t, id, "thumbnail", nil, "thumbnails/"+tc.wantState+".jpg", "active")
			copyID := f.CopyID(t, asset)
			marked, err := transfer.MarkMediaAssetCopyCorrupt(ctx, id, asset, copyID, "tokyo")
			if err != nil || marked != tc.wantMarked {
				t.Errorf("MarkMediaAssetCopyCorrupt = %v, %v; want %v, nil", marked, err, tc.wantMarked)
			}
			got, ok, err := transfer.GetMediaAssetCopyForDeletion(ctx, store.MediaAssetCopyDeleteCandidate{
				CopyID: copyID, MediaAssetID: asset, Store: "tokyo",
			})
			if err != nil || !ok || got.State != tc.wantState {
				t.Errorf("copy after retirement decision = %+v, %v, %v; want state %q", got, ok, err, tc.wantState)
			}
		})
	}
}
