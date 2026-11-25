package contracttest

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// runCatalogRestoreLegacyFailureReported は、failureReported の無い旧形式の文書を Restore が
// 品質イベントの配列から列を埋めることを確かめる。
func runCatalogRestoreLegacyFailureReported(t *testing.T, newStore NewCatalogStore) {
	tests := []struct {
		name   string
		events string
		want   bool
	}{
		{name: "failed event", events: `[{"event":"recording.failed"}]`, want: true},
		{name: "other event", events: `[{"event":"recording.record-broken"}]`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newStore(t)
			ctx := context.Background()
			doc := roundTripCatalogDocument()
			doc.Recordings[0].FailureReported = nil
			doc.Recordings[0].QualityEvents = []byte(tt.events)
			if _, err := f.Store.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
				t.Fatalf("Restore of a legacy document: %v", err)
			}
			exported, err := f.Store.Export(ctx)
			if err != nil {
				t.Fatalf("Export after restore: %v", err)
			}
			got := exportedRecording(t, exported, 30)
			if got.FailureReported == nil || *got.FailureReported != tt.want {
				t.Fatalf("restored failureReported = %v, want %t", got.FailureReported, tt.want)
			}
		})
	}
}

// runCatalogRestoreKeepsCurrentGenerationAgainstOlderCatalog は、古い世代の文書が
// 論理資産の現行世代・サイズを戻さないことを確かめる。
func runCatalogRestoreKeepsCurrentGenerationAgainstOlderCatalog(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	seed := roundTripCatalogDocument()
	if _, err := f.Store.Restore(ctx, seed, store.RestoreOptions{}); err != nil {
		t.Fatalf("seeding Restore: %v", err)
	}
	current, older := int64(8), int64(7)
	profile := "h264"
	createdAt := seed.MediaAssets[1].CreatedAt
	restoreAsset := func(asset catalogdoc.MediaAsset) {
		t.Helper()
		doc := &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{asset}}
		if _, err := f.Store.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
			t.Fatalf("Restore of asset %d generation %d: %v", asset.ID, *asset.Generation, err)
		}
	}
	restoreAsset(catalogdoc.MediaAsset{
		ID: 41, RecordingID: 30, Kind: "encoded", Profile: &profile, RelPath: "sites/default/round-trip.g8.mp4",
		SizeBytes: 100, State: "active", CreatedAt: createdAt, UpdatedAt: createdAt,
		DerivedGeneration: &current, Generation: &current, Store: "test-store",
	})
	restoreAsset(catalogdoc.MediaAsset{
		ID: 41, RecordingID: 30, Kind: "encoded", Profile: &profile, RelPath: "sites/default/round-trip.g7.mp4",
		SizeBytes: 80, State: "active", CreatedAt: createdAt, UpdatedAt: createdAt,
		DerivedGeneration: &older, Generation: &older, Store: "test-store",
	})
	exported, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	seen := 0
	for _, asset := range exported.MediaAssets {
		if asset.ID != 41 {
			continue
		}
		seen++
		if asset.DerivedGeneration == nil || *asset.DerivedGeneration != current || asset.SizeBytes != 100 {
			t.Fatalf("logical asset 41 after an older catalog = derived %v, size %d; want generation %d, size 100",
				asset.DerivedGeneration, asset.SizeBytes, current)
		}
	}
	if seen == 0 {
		t.Fatal("asset 41 is missing after restoring an older catalog")
	}
}

// runCatalogRestoreKeepsLiveOwnerAgainstTombstone は、同じ identity の墓石を復元しても、
// そのパスの生きたコピーを墓石に置き換えないことを確かめる。墓石は別の（合成の）パスに置かれる。
func runCatalogRestoreKeepsLiveOwnerAgainstTombstone(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	seed := roundTripCatalogDocument()
	if _, err := f.Store.Restore(ctx, seed, store.RestoreOptions{}); err != nil {
		t.Fatalf("seeding Restore: %v", err)
	}
	live := seed.MediaAssets[0]
	tombstone := live
	deletedAt := live.CreatedAt
	tombstone.State, tombstone.DeletedAt = "deleted", &deletedAt
	doc := &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{live, tombstone}}
	if _, err := f.Store.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatalf("Restore of a live copy and its tombstone: %v", err)
	}
	exported, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var active, deleted int
	for _, asset := range exported.MediaAssets {
		if asset.ID != live.ID {
			continue
		}
		switch asset.State {
		case "active":
			active++
			if asset.RelPath != live.RelPath {
				t.Fatalf("live copy path = %q, want %q", asset.RelPath, live.RelPath)
			}
		case "deleted":
			deleted++
			if asset.RelPath == live.RelPath {
				t.Fatalf("tombstone took the live copy's path %q", live.RelPath)
			}
		}
	}
	if active != 1 || deleted != 1 {
		t.Fatalf("copies of asset %d = %d active, %d deleted; want 1 active and 1 tombstone", live.ID, active, deleted)
	}
}

// exportedRecording は文書の録画を ID で返す。無ければ検査を失敗させる。
func exportedRecording(t *testing.T, doc *catalogdoc.Document, id int64) catalogdoc.Recording {
	t.Helper()
	for _, r := range doc.Recordings {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("recording %d is missing from the export", id)
	return catalogdoc.Recording{}
}
