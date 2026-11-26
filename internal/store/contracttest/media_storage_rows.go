package contracttest

import (
	"errors"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// runStorageObservationRows は観測表の行を置き場ごとに読み、消す主張を検査する。
// 置き場に束縛した書き手は自分の置き場の行だけを読み、消し、他の置き場の行を消さない。
func runStorageObservationRows(t *testing.T, newFixture NewMediaFixture) {
	t.Run("storage roots not requested are pruned within their store", func(t *testing.T) { runStorageRootPrune(t, newFixture) })
	t.Run("ingest original rel_path is read regardless of copy state", func(t *testing.T) { runIngestOriginalRelPath(t, newFixture) })
	t.Run("active media assets are listed per store", func(t *testing.T) { runActiveMediaAssetsByStore(t, newFixture) })
	t.Run("missing asset candidates are listed and cleared per store", func(t *testing.T) { runMissingAssetCandidatesByStore(t, newFixture) })
}

// runStorageRootPrune は今回の config が要求しない root だけを、束縛した置き場の中で消すことを検査する。
func runStorageRootPrune(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	tokyo := f.OpenStore("tokyo").Storage
	takamatsu := f.OpenStore("takamatsu").Storage
	for _, row := range []struct {
		storage store.StorageObservation
		input   store.StorageObservationInput
	}{
		{tokyo, store.StorageObservationInput{Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 1, AvailableBytes: 99}},
		{tokyo, store.StorageObservationInput{Root: "scratch", Path: "/scratch", TotalBytes: 100, UsedBytes: 2, AvailableBytes: 98}},
		{takamatsu, store.StorageObservationInput{Root: "media", Path: "/mnt/takamatsu", TotalBytes: 100, UsedBytes: 3, AvailableBytes: 97}},
		{takamatsu, store.StorageObservationInput{Root: "scratch", Path: "/mnt/takamatsu-scratch", TotalBytes: 100, UsedBytes: 4, AvailableBytes: 96}},
	} {
		if err := row.storage.UpsertStorageSync(ctx, row.input); err != nil {
			t.Fatalf("UpsertStorageSync(%s): %v", row.input.Root, err)
		}
	}

	if err := tokyo.DeleteStorageSyncExcept(ctx, []string{"media"}); err != nil {
		t.Fatalf("DeleteStorageSyncExcept(tokyo, media): %v", err)
	}
	checkStorageRoots(t, f, "after pruning tokyo to media", map[string]bool{
		"tokyo/media": true, "takamatsu/media": true, "takamatsu/scratch": true,
	})

	// 空の集合は、その置き場の全行を消す（他の置き場は残す）。
	if err := takamatsu.DeleteStorageSyncExcept(ctx, nil); err != nil {
		t.Fatalf("DeleteStorageSyncExcept(takamatsu, nil): %v", err)
	}
	checkStorageRoots(t, f, "after clearing takamatsu", map[string]bool{"tokyo/media": true})
}

// checkStorageRoots は束縛なしの監視読み取りが返す観測行の「置き場/root」の集合を want と比べる。
func checkStorageRoots(t *testing.T, f MediaFixture, when string, want map[string]bool) {
	t.Helper()
	rows, err := f.OpenStore("").StorageSync.ListStorageSync(t.Context())
	if err != nil {
		t.Fatalf("ListStorageSync (%s): %v", when, err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.Store+"/"+row.Root] = true
	}
	if len(got) != len(want) {
		t.Fatalf("ListStorageSync (%s) = %v, want %v", when, got, want)
	}
	for key := range want {
		if !got[key] {
			t.Fatalf("ListStorageSync (%s) = %v, want %v", when, got, want)
		}
	}
}

// runIngestOriginalRelPath は取り込みの転送前確認が、原本の rel_path を状態に関係なく返し、原本が無ければ ErrNotFound を返すことを検査する。
func runIngestOriginalRelPath(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.Precheck.GetIngestOriginalRelPath(ctx, f.NewRecording(t)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetIngestOriginalRelPath without an original = %v, want ErrNotFound", err)
	}
	for _, state := range []string{"active", "deleting", "deleted"} {
		recordingID := f.NewRecording(t)
		relPath := "sites/default/ingest-original-" + state + ".m2ts"
		f.NewAsset(t, recordingID, "original", nil, relPath, state)
		got, err := f.Precheck.GetIngestOriginalRelPath(ctx, recordingID)
		if err != nil || got != relPath {
			t.Fatalf("GetIngestOriginalRelPath with a %s original = (%q, %v), want (%q, nil)", state, got, err, relPath)
		}
	}
}

// runActiveMediaAssetsByStore は実体無しの差集合に使う active コピーが、束縛した置き場のものだけであることを検査する。
func runActiveMediaAssetsByStore(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	activeRecording, deletingRecording, deletedRecording := f.NewRecording(t), f.NewRecording(t), f.NewRecording(t)
	activePath := "sites/default/active-listed.m2ts"
	activeAssetID := f.NewAsset(t, activeRecording, "original", nil, activePath, "active")
	f.NewAsset(t, deletingRecording, "original", nil, "sites/default/deleting-listed.m2ts", "deleting")
	f.NewAsset(t, deletedRecording, "original", nil, "sites/default/deleted-listed.m2ts", "deleted")
	takamatsuPath := "sites/default/active-listed-takamatsu.m2ts"
	f.AddCopy(t, activeAssetID, "takamatsu", takamatsuPath)

	tokyo, err := f.OpenStore("tokyo").Storage.ListActiveMediaAssets(ctx)
	if err != nil {
		t.Fatalf("ListActiveMediaAssets(tokyo): %v", err)
	}
	want := store.ActiveMediaAsset{
		ID: f.CopyID(t, activeAssetID), MediaAssetID: activeAssetID, RecordingID: activeRecording,
		RelPath: activePath, Kind: "original",
	}
	if len(tokyo) != 1 || tokyo[0] != want {
		t.Fatalf("ListActiveMediaAssets(tokyo) = %+v, want only %+v", tokyo, want)
	}

	takamatsu, err := f.OpenStore("takamatsu").Storage.ListActiveMediaAssets(ctx)
	if err != nil {
		t.Fatalf("ListActiveMediaAssets(takamatsu): %v", err)
	}
	if len(takamatsu) != 1 || takamatsu[0].RelPath != takamatsuPath || takamatsu[0].MediaAssetID != activeAssetID {
		t.Fatalf("ListActiveMediaAssets(takamatsu) = %+v, want only the takamatsu copy at %q", takamatsu, takamatsuPath)
	}
}

// runMissingAssetCandidatesByStore は実体無し候補の ID 一覧と消去が、束縛した置き場の候補だけに効くことを検査する。
func runMissingAssetCandidatesByStore(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	assetID := f.NewAsset(t, f.NewRecording(t), "original", nil, "sites/default/missing-listed.m2ts", "active")
	tokyoCopy := f.CopyID(t, assetID)
	takamatsuCopy := f.AddCopy(t, assetID, "takamatsu", "sites/default/missing-listed-takamatsu.m2ts")
	tokyo := f.OpenStore("tokyo").Storage
	takamatsu := f.OpenStore("takamatsu").Storage
	if err := tokyo.UpsertMissingMediaAsset(ctx, tokyoCopy); err != nil {
		t.Fatalf("UpsertMissingMediaAsset(tokyo): %v", err)
	}
	if err := takamatsu.UpsertMissingMediaAsset(ctx, takamatsuCopy); err != nil {
		t.Fatalf("UpsertMissingMediaAsset(takamatsu): %v", err)
	}

	check := func(when string, s store.StorageObservation, want ...int64) {
		t.Helper()
		got, err := s.ListAllMissingMediaAssetIDs(ctx)
		slices.Sort(got)
		slices.Sort(want)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("ListAllMissingMediaAssetIDs (%s) = %v, %v; want %v", when, got, err, want)
		}
	}
	check("tokyo", tokyo, tokyoCopy)
	check("takamatsu", takamatsu, takamatsuCopy)

	if err := tokyo.DeleteMissingMediaAsset(ctx, tokyoCopy); err != nil {
		t.Fatalf("DeleteMissingMediaAsset(tokyo): %v", err)
	}
	check("tokyo after the candidate was observed again", tokyo)
	check("takamatsu after tokyo was cleared", takamatsu, takamatsuCopy)
}
