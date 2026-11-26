package contracttest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// MediaStoreScope は同じ DB の指定置き場に対する読み取りと観測の契約をまとめる。
// 空の置き場は束縛のない読み取り用とする。
type MediaStoreScope struct {
	Locator     store.OriginalLocator
	Storage     store.StorageObservation
	StorageSync store.StorageSyncReader
	Publisher   store.DerivedMediaPublisher
	Reclaim     store.PendingCopyReclaim
}

func runOriginalStoreRouting(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	onlyTakamatsu, both, gone, unrelated := f.NewRecording(t), f.NewRecording(t), f.NewRecording(t), f.NewRecording(t)
	onlyAsset := f.NewAsset(t, onlyTakamatsu, "original", nil, "a.m2ts", "deleted")
	f.AddCopy(t, onlyAsset, "takamatsu", "tk/a.m2ts")
	bothAsset := f.NewAsset(t, both, "original", nil, "b.m2ts", "active")
	f.AddCopy(t, bothAsset, "osaka", "osaka/b.m2ts")
	f.NewAsset(t, gone, "original", nil, "c.m2ts", "deleted")
	f.NewAsset(t, unrelated, "original", nil, "d.m2ts", "active")

	// 原本を読むジョブは、束縛によらず、複数の置き場のうち名前順の先頭へ送る。
	want := map[int64]string{onlyTakamatsu: "takamatsu", both: "osaka"}
	for _, scope := range []string{"", "tokyo"} {
		locator := f.OpenStore(scope).Locator
		got, err := locator.OriginalStores(ctx, []int64{onlyTakamatsu, both, gone})
		if err != nil || !maps.Equal(got, want) {
			t.Errorf("OriginalStores in scope %q = %v, %v; want %v", scope, got, err, want)
		}
		for id, wantStore := range want {
			if got, err := locator.OriginalStore(ctx, id); err != nil || got != wantStore {
				t.Errorf("OriginalStore(%d) in scope %q = %q, %v; want %q", id, scope, got, err, wantStore)
			}
		}
		// コピーが消えた録画は一括結果から省かれ、単独では ErrNotFound になる。
		if _, err := locator.OriginalStore(ctx, gone); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("OriginalStore for deleted copy in scope %q = %v, want ErrNotFound", scope, err)
		}
		if got, err := locator.OriginalStores(ctx, nil); err != nil || len(got) != 0 {
			t.Errorf("OriginalStores(nil) in scope %q = %v, %v; want empty", scope, got, err)
		}
	}
}

func runOriginalServingInStore(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	id := f.NewRecording(t)
	assetID := f.NewAsset(t, id, "original", nil, "tokyo/x.m2ts", "deleted")
	f.AddCopy(t, assetID, "takamatsu", "tk/x.m2ts")

	// 置き場ごとの URL は指定された置き場のパスを返す。
	got, err := f.Serving.GetOriginalMediaAssetForServingInStore(ctx, id, "takamatsu")
	if err != nil || got.ID != assetID || got.Store != "takamatsu" || got.RelPath != "tk/x.m2ts" {
		t.Fatalf("serving from takamatsu = %+v, %v; want asset %d at takamatsu/tk/x.m2ts", got, err, assetID)
	}
	for _, tc := range []struct {
		name  string
		id    int64
		place string
	}{
		{"deleted copy", id, "tokyo"},
		{"no copy", id, "osaka"},
	} {
		if _, err := f.Serving.GetOriginalMediaAssetForServingInStore(ctx, tc.id, tc.place); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("serving %s: %v, want ErrNotFound", tc.name, err)
		}
	}
	for _, state := range []string{"pending", "deleting"} {
		other := f.NewRecording(t)
		f.NewAsset(t, other, "original", nil, fmt.Sprintf("%s.m2ts", state), state)
		if _, err := f.Serving.GetOriginalMediaAssetForServingInStore(ctx, other, "tokyo"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("serving %s copy: %v, want ErrNotFound", state, err)
		}
	}
	f.SetRecordingTrashed(t, id)
	if _, err := f.Serving.GetOriginalMediaAssetForServingInStore(ctx, id, "takamatsu"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("serving trashed recording: %v, want ErrNotFound", err)
	}
}

func runDesiredOriginalStore(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	// 年齢 0 では older_than を指定した先頭規則を飛ばし、site と無条件の規則を順に評価する。
	rules := []store.OriginalStoreRule{
		{Site: "takamatsu", OlderThan: 30 * 24 * time.Hour, Store: "nagoya"},
		{Site: "takamatsu", Store: "takamatsu"},
		{Store: "tokyo"},
	}
	for _, tc := range []struct {
		name       string
		rules      []store.OriginalStoreRule
		site, want string
	}{
		{"matching site", rules, "takamatsu", "takamatsu"},
		{"fallback", rules, "home", "tokyo"},
		{"no matching rule", rules[:2], "home", ""},
		{"no rules", nil, "home", ""},
	} {
		chooser := f.NewIngestStoreChooser(store.NewStoragePolicy(tc.rules, []string{"tokyo", "takamatsu", "nagoya"}, "tokyo"))
		got, err := chooser.DesiredOriginalStoreAtIngest(t.Context(), tc.site)
		if err != nil || got != tc.want {
			t.Errorf("%s: DesiredOriginalStoreAtIngest(%q) = %q, %v; want %q", tc.name, tc.site, got, err, tc.want)
		}
	}
}

func runIngestWriterState(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	id, otherSiteID, noCheckpointID := f.NewRecording(t), f.NewRecording(t), f.NewRecording(t)
	f.NewRecordSync(t, id, f.Site, "same-record", "recording")
	f.NewRecordSync(t, otherSiteID, "remote", "same-record", "recording")
	f.NewRecordSync(t, noCheckpointID, f.Site, "no-checkpoint", "recording")
	f.SetIngestCheckpoint(t, id, 19)
	f.SetIngestCheckpoint(t, otherSiteID, 7)
	f.SetIngestDisplayPath(t, id, "sites/default/.rokuban-ingest-older")

	// 孤児回収には、現在の writer 世代と、まだ表示している以前の temp の両方が必要。
	for _, tc := range []struct {
		site, record string
		found        bool
		want         store.IngestWriterState
	}{
		{f.Site, "same-record", true, store.IngestWriterState{RecordingID: id, WriterGeneration: 19, HasProgress: true, TempRelPath: "sites/default/.rokuban-ingest-older"}},
		{"remote", "same-record", true, store.IngestWriterState{RecordingID: otherSiteID, WriterGeneration: 7}},
		{"another-site", "same-record", false, store.IngestWriterState{}},
		{f.Site, "no-checkpoint", false, store.IngestWriterState{}},
		{f.Site, "unknown", false, store.IngestWriterState{}},
	} {
		got, found, err := f.WriterState.GetIngestWriterState(ctx, tc.site, tc.record)
		if err != nil || found != tc.found || (found && got != tc.want) {
			t.Errorf("GetIngestWriterState(%q, %q) = %+v, %v, %v; want %+v, %v, nil", tc.site, tc.record, got, found, err, tc.want, tc.found)
		}
	}
}

func runStorageSyncScope(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	for _, row := range []struct {
		place string
		input store.StorageObservationInput
	}{
		{"tokyo", store.StorageObservationInput{Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75}},
		{"tokyo", store.StorageObservationInput{Root: "scratch", Path: "/scratch", TotalBytes: 200, UsedBytes: 60, AvailableBytes: 140}},
		{"takamatsu", store.StorageObservationInput{Root: "media", Path: "/mnt/takamatsu", TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90}},
	} {
		if err := f.OpenStore(row.place).Storage.UpsertStorageSync(ctx, row.input); err != nil {
			t.Fatalf("UpsertStorageSync(%s, %s): %v", row.place, row.input.Root, err)
		}
	}

	check := func(scope string, rows []store.StorageSync, err error, want map[[2]string]store.StorageObservationInput) {
		t.Helper()
		if err != nil || len(rows) != len(want) {
			t.Fatalf("ListStorageSync in scope %q = %+v, %v; want %v", scope, rows, err, want)
		}
		for _, row := range rows {
			key := [2]string{row.Store, row.Root}
			payload, ok := want[key]
			if !ok || row.Path != payload.Path || row.TotalBytes != payload.TotalBytes ||
				row.UsedBytes != payload.UsedBytes || row.AvailableBytes != payload.AvailableBytes || row.ObservedAt.IsZero() {
				t.Errorf("ListStorageSync in scope %q: unexpected observation %+v, expected %+v with timestamp", scope, row, payload)
			}
			delete(want, key)
		}
	}
	// 監視は全置き場の状態を読み、置き場に束縛した読み取りは自分の観測だけを見る。
	rows, err := f.OpenStore("").StorageSync.ListStorageSync(ctx)
	check("", rows, err, map[[2]string]store.StorageObservationInput{
		{"tokyo", "media"}:     {Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75},
		{"tokyo", "scratch"}:   {Path: "/scratch", TotalBytes: 200, UsedBytes: 60, AvailableBytes: 140},
		{"takamatsu", "media"}: {Path: "/mnt/takamatsu", TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
	})
	rows, err = f.OpenStore("takamatsu").Storage.ListStorageSync(ctx)
	check("takamatsu", rows, err, map[[2]string]store.StorageObservationInput{
		{"takamatsu", "media"}: {Path: "/mnt/takamatsu", TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
	})
}

func runOrphanStoreScope(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	path := "2026/07/orphan.m2ts"
	tokyo := f.OpenStore("tokyo").Storage
	takamatsu := f.OpenStore("takamatsu").Storage
	for _, media := range []store.StorageObservation{tokyo, takamatsu} {
		if err := media.UpsertOrphanFile(ctx, path); err != nil {
			t.Fatalf("UpsertOrphanFile: %v", err)
		}
	}
	if err := tokyo.DeleteOrphanFile(ctx, path); err != nil {
		t.Fatalf("deleting Tokyo orphan record: %v", err)
	}
	if rows, err := tokyo.ListAllOrphanFiles(ctx); err != nil || len(rows) != 0 {
		t.Errorf("Tokyo orphans = %+v, %v; want none", rows, err)
	}
	if rows, err := takamatsu.ListAllOrphanFiles(ctx); err != nil || len(rows) != 1 || rows[0].RelPath != "2026/07/orphan.m2ts" {
		t.Errorf("Takamatsu orphans = %+v, %v; want the Takamatsu orphan", rows, err)
	}
}

func observeMissingContractCopy(t *testing.T, f MediaFixture, assetID int64) {
	t.Helper()
	f.SetCopyMissing(t, f.CopyID(t, assetID), true)
}

func runMissingDerivativeCandidates(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	present, missing := f.NewRecording(t), f.NewRecording(t)
	f.NewAsset(t, present, "original", nil, "originals/present.ts", "active")
	observeMissingContractCopy(t, f, f.NewAsset(t, missing, "original", nil, "originals/missing.ts", "active"))
	// 派生物が既にある録画は、その kind の候補から外れ、もう一方の kind の候補には残る。
	hasThumbnail, hasTiles := f.NewRecording(t), f.NewRecording(t)
	f.NewAsset(t, hasThumbnail, "original", nil, "originals/has-thumbnail.ts", "active")
	f.NewAsset(t, hasThumbnail, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", hasThumbnail), "active")
	f.NewAsset(t, hasTiles, "original", nil, "originals/has-tiles.ts", "active")
	f.NewAsset(t, hasTiles, "seek_tiles", nil, fmt.Sprintf("thumbnails/%d_tiles.jpg", hasTiles), "active")
	for _, tc := range []struct {
		name    string
		list    func(context.Context, store.MediaReconcilePage) ([]int64, error)
		include []int64
		exclude []int64
	}{
		{"thumbnail", f.Reconcile.ListMissingThumbnailRecordings, []int64{present, hasTiles}, []int64{hasThumbnail, missing}},
		{"seek tiles", f.Reconcile.ListMissingSeekTilesRecordings, []int64{present, hasThumbnail}, []int64{hasTiles, missing}},
	} {
		rows, err := tc.list(t.Context(), store.MediaReconcilePage{RowLimit: 10})
		if err != nil {
			t.Fatalf("%s candidates: %v", tc.name, err)
		}
		for _, id := range tc.include {
			if !slices.Contains(rows, id) {
				t.Errorf("%s candidates = %v; want recording %d included", tc.name, rows, id)
			}
		}
		for _, id := range tc.exclude {
			if slices.Contains(rows, id) {
				t.Errorf("%s candidates = %v; want recording %d excluded", tc.name, rows, id)
			}
		}
	}
}
