// Package contracttest は store の契約を実装に対して検査する統合テストを持つ。
package contracttest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// MediaFixture はメディア契約の実装と状態を作るフックをまとめる。
type MediaFixture struct {
	Serving   store.MediaServing
	Reconcile store.MediaReconcile
	Storage   store.StorageObservation

	// Site は NewRecording が作る録画の site。
	Site string

	NewRecording                  func(t *testing.T) int64
	NewAsset                      func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64
	SetAssetState                 func(t *testing.T, assetID int64, state string)
	SetMediaAssetKeepRanges       func(t *testing.T, assetID int64, keep []chapters.Range)
	SetEncodedAssetRebuildRequest func(t *testing.T, recordingID int64, profile string, keep []chapters.Range)
	// SetRecordingTrashed は録画をごみ箱に入れる（deleted_at を埋める）。
	SetRecordingTrashed func(t *testing.T, recordingID int64)
	// NewRecordSync は録画に対応する mirakc record の同期行を作る。
	NewRecordSync func(t *testing.T, recordingID int64, site, recordID, status string)
	// SetOrphanFirstSeenAgo は孤児候補の first_seen を DB の now() から ago 前へずらす。
	SetOrphanFirstSeenAgo func(t *testing.T, relPath string, ago time.Duration)
	// SetMissingFirstSeenAgo は実体無し候補の first_seen を DB の now() から ago 前へずらす。
	SetMissingFirstSeenAgo func(t *testing.T, assetID int64, ago time.Duration)
}

// NewMediaFixture は実在の契約実装とフィクスチャを作る。
type NewMediaFixture func(t *testing.T) MediaFixture

// RunMedia は実在のメディア store 実装に対して配信・名前空間・監査の契約を実行する。
func RunMedia(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("media jobs read active inputs through their contract", func(t *testing.T) { runMediaJobInputs(t, newFixture) })
	t.Run("encode planning reads asset and request state through its contract", func(t *testing.T) { runMediaEncodePlanning(t, newFixture) })
	t.Run("serving returns only active assets", func(t *testing.T) { runMediaServingActiveOnly(t, newFixture) })
	t.Run("serving covers thumbnails and seek tiles", func(t *testing.T) { runMediaServingDerived(t, newFixture) })
	t.Run("original VOD target is scoped to the recording's site", func(t *testing.T) { runMediaOriginalVODTarget(t, newFixture) })
	t.Run("chase target requires a record and a recording outside the trash", func(t *testing.T) { runMediaChaseTarget(t, newFixture) })
	t.Run("namespace guard accepts site paths and rejects malformed ones", func(t *testing.T) { runMediaNamespaceMalformed(t, newFixture) })
	t.Run("namespace guard rejects unprefixed originals and encoded files", func(t *testing.T) { runMediaNamespaceUnprefixed(t, newFixture) })
	t.Run("namespace guard leaves thumbnails and seek tiles to their own namespace", func(t *testing.T) { runMediaNamespaceDerived(t, newFixture) })
	t.Run("audit lists every media asset path including reserved namespaces", func(t *testing.T) { runMediaAuditPaths(t, newFixture) })
	t.Run("orphan ageing compares against the database clock", func(t *testing.T) { runMediaOrphanAgeing(t, newFixture) })
	t.Run("missing asset ageing compares against the database clock and skips inactive rows", func(t *testing.T) { runMediaMissingAgeing(t, newFixture) })
}

func runMediaJobInputs(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	if _, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveOriginalMediaAsset(%d) error = %v, want ErrNotFound", id, err)
	}
	if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasActiveThumbnailMediaAsset(%d) = (%v, %v), want (false, nil)", id, has, err)
	}

	originalPath := fmt.Sprintf("sites/home/%d/original.m2ts", id)
	originalID := f.NewAsset(t, id, "original", nil, originalPath, "active")
	original, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset(%d): %v", id, err)
	}
	if original.ID != originalID || original.RelPath != originalPath {
		t.Fatalf("active original = %+v, want id %d and path %q", original, originalID, originalPath)
	}

	thumbnailID := f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
	if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || !has {
		t.Fatalf("HasActiveThumbnailMediaAsset(%d) = (%v, %v), want (true, nil)", id, has, err)
	}
	f.SetAssetState(t, thumbnailID, "deleted")
	if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasActiveThumbnailMediaAsset(%d) after deletion = (%v, %v), want (false, nil)", id, has, err)
	}
	f.SetAssetState(t, originalID, "deleted")
	if _, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveOriginalMediaAsset(%d) after deletion error = %v, want ErrNotFound", id, err)
	}
}

func runMediaEncodePlanning(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	runMediaEncodePlanningContract(t, f)
}

func runMediaServingActiveOnly(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	for _, state := range []string{"deleting", "deleted"} {
		t.Run("original "+state, func(t *testing.T) {
			id := f.NewRecording(t)
			f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/home/%d/original.m2ts", id), state)
			if _, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetOriginalMediaAssetForServing(%d) error = %v, want ErrNotFound", id, err)
			}
		})
		t.Run("encoded "+state, func(t *testing.T) {
			id := f.NewRecording(t)
			profile := "h264"
			f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/encoded.mp4", id), state)
			if _, err := f.Serving.GetEncodedMediaAssetForServing(ctx, id, profile); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetEncodedMediaAssetForServing(%d, %q) error = %v, want ErrNotFound", id, profile, err)
			}
		})
	}

	id := f.NewRecording(t)
	path := "sites/home/active.m2ts"
	assetID := f.NewAsset(t, id, "original", nil, path, "active")
	asset, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id)
	if err != nil {
		t.Fatalf("GetOriginalMediaAssetForServing(%d): %v", id, err)
	}
	if asset.ID != assetID || asset.RelPath != path {
		t.Fatalf("served original = %+v, want id %d and path %q", asset, assetID, path)
	}
}

func runMediaServingDerived(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	kinds := []struct {
		kind string
		path func(id int64) string
		get  func(id int64) (store.MediaAsset, error)
	}{
		{"thumbnail", func(id int64) string { return fmt.Sprintf("thumbnails/%d.jpg", id) },
			func(id int64) (store.MediaAsset, error) { return f.Serving.GetThumbnailMediaAssetForServing(ctx, id) }},
		{"seek_tiles", func(id int64) string { return fmt.Sprintf("thumbnails/%d_tiles.jpg", id) },
			func(id int64) (store.MediaAsset, error) { return f.Serving.GetSeekTilesMediaAssetForServing(ctx, id) }},
	}
	for _, k := range kinds {
		for _, state := range []string{"deleting", "deleted"} {
			t.Run(k.kind+" "+state, func(t *testing.T) {
				id := f.NewRecording(t)
				f.NewAsset(t, id, k.kind, nil, k.path(id), state)
				if _, err := k.get(id); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("%s %s error = %v, want ErrNotFound", k.kind, state, err)
				}
			})
		}
		t.Run(k.kind+" of a trashed recording", func(t *testing.T) {
			id := f.NewRecording(t)
			f.NewAsset(t, id, k.kind, nil, k.path(id), "active")
			f.SetRecordingTrashed(t, id)
			if _, err := k.get(id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("trashed %s error = %v, want ErrNotFound", k.kind, err)
			}
		})
		t.Run(k.kind+" active", func(t *testing.T) {
			id := f.NewRecording(t)
			assetID := f.NewAsset(t, id, k.kind, nil, k.path(id), "active")
			got, err := k.get(id)
			if err != nil {
				t.Fatalf("%s: %v", k.kind, err)
			}
			if got.ID != assetID || got.RelPath != k.path(id) {
				t.Fatalf("%s = %+v, want id %d path %q", k.kind, got, assetID, k.path(id))
			}
		})
	}
}

func runMediaOriginalVODTarget(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	assetID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
	got, err := f.Serving.GetOriginalVODTarget(ctx, id, f.Site)
	if err != nil {
		t.Fatalf("GetOriginalVODTarget(own site): %v", err)
	}
	if got.ID != assetID || got.Site != f.Site {
		t.Fatalf("target = %+v, want asset %d in site %q", got, assetID, f.Site)
	}
	if _, err := f.Serving.GetOriginalVODTarget(ctx, id, f.Site+"-other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOriginalVODTarget(other site) error = %v, want ErrNotFound", err)
	}
	f.SetRecordingTrashed(t, id)
	if _, err := f.Serving.GetOriginalVODTarget(ctx, id, f.Site); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOriginalVODTarget(trashed) error = %v, want ErrNotFound", err)
	}
}

func runMediaChaseTarget(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	if _, err := f.Serving.GetChaseTarget(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetChaseTarget without record_sync error = %v, want ErrNotFound", err)
	}
	f.NewRecordSync(t, id, f.Site, "record-1", "recording")
	got, err := f.Serving.GetChaseTarget(ctx, id)
	if err != nil {
		t.Fatalf("GetChaseTarget: %v", err)
	}
	if got.Site != f.Site || got.RecordID != "record-1" || got.Status != "recording" || got.RecordingStatus != "finished" || got.HasOriginalAsset {
		t.Fatalf("chase target = %+v, want site %q record-1 recording/finished without an original", got, f.Site)
	}
	f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "deleted")
	if got, err := f.Serving.GetChaseTarget(ctx, id); err != nil || !got.HasOriginalAsset {
		t.Fatalf("GetChaseTarget with a deleted original = (%+v, %v), want HasOriginalAsset regardless of state", got, err)
	}
	f.SetRecordingTrashed(t, id)
	if _, err := f.Serving.GetChaseTarget(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetChaseTarget of a trashed recording error = %v, want ErrNotFound", err)
	}
}

func runMediaNamespaceMalformed(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	validID := f.NewRecording(t)
	f.NewAsset(t, validID, "original", nil, "sites/home/archive/show.m2ts", "active")
	if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx); err != nil || violation != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath() = (%+v, %v), want (nil, nil)", violation, err)
	}

	invalidID := f.NewRecording(t)
	assetID := f.NewAsset(t, invalidID, "original", nil, "sites//archive/show.m2ts", "active")
	violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx)
	if err != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath(): %v", err)
	}
	if violation == nil || violation.ID != assetID || violation.RelPath != "sites//archive/show.m2ts" {
		t.Fatalf("violation = %+v, want asset %d with malformed site path", violation, assetID)
	}
	f.SetAssetState(t, assetID, "deleted")
	if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx); err != nil || violation != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath() after deleting malformed row = (%+v, %v), want (nil, nil)", violation, err)
	}
}

func runMediaNamespaceUnprefixed(t *testing.T, newFixture NewMediaFixture) {
	const unprefixed = "20240101/show.m2ts"
	for _, kind := range []string{"original", "encoded"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			var profile *string
			if kind == "encoded" {
				p := "h264"
				profile = &p
			}
			assetID := f.NewAsset(t, f.NewRecording(t), kind, profile, unprefixed, "active")
			violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(context.Background())
			if err != nil {
				t.Fatalf("FindInvalidMediaAssetRelPath(): %v", err)
			}
			if violation == nil || violation.ID != assetID || violation.Kind != kind || violation.RelPath != unprefixed {
				t.Fatalf("violation = %+v, want %s asset %d at %q", violation, kind, assetID, unprefixed)
			}
		})
	}
}

func runMediaNamespaceDerived(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	id := f.NewRecording(t)
	f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
	f.NewAsset(t, id, "seek_tiles", nil, fmt.Sprintf("thumbnails/%d_tiles.jpg", id), "active")
	if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(context.Background()); err != nil || violation != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath() = (%+v, %v), want (nil, nil) for thumbnails/ paths", violation, err)
	}
}

func runMediaAuditPaths(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	want := []string{
		"sites/home/archive/20240101/show.m2ts",
		fmt.Sprintf("thumbnails/%d.jpg", id),
		fmt.Sprintf("thumbnails/%d_tiles.jpg", id),
	}
	f.NewAsset(t, id, "original", nil, want[0], "deleted")
	f.NewAsset(t, id, "thumbnail", nil, want[1], "active")
	f.NewAsset(t, id, "seek_tiles", nil, want[2], "active")
	paths, err := f.Storage.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		t.Fatalf("ListAllMediaAssetRelPaths(): %v", err)
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[p] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("ListAllMediaAssetRelPaths() = %q, want it to include %q", paths, w)
		}
	}
}

func runMediaOrphanAgeing(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	const old, fresh = "sites/home/old.tmp", "sites/home/fresh.tmp"
	for _, p := range []string{old, fresh} {
		if err := f.Storage.UpsertOrphanFile(ctx, p); err != nil {
			t.Fatalf("UpsertOrphanFile(%q): %v", p, err)
		}
	}
	f.SetOrphanFirstSeenAgo(t, old, 2*time.Hour)
	got, err := f.Storage.ListAgedOrphanFiles(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ListAgedOrphanFiles(1h): %v", err)
	}
	if len(got) != 1 || got[0] != old {
		t.Fatalf("ListAgedOrphanFiles(1h) = %q, want only %q", got, old)
	}
	got, err = f.Storage.ListAgedOrphanFiles(ctx, 3*time.Hour)
	if err != nil {
		t.Fatalf("ListAgedOrphanFiles(3h): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListAgedOrphanFiles(3h) = %q, want none", got)
	}
}

func runMediaMissingAgeing(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	assetID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/home/%d/original.m2ts", id), "active")
	if err := f.Storage.UpsertMissingMediaAsset(ctx, assetID); err != nil {
		t.Fatalf("UpsertMissingMediaAsset(%d): %v", assetID, err)
	}
	f.SetMissingFirstSeenAgo(t, assetID, 2*time.Hour)
	got, err := f.Storage.ListAgedMissingMediaAssets(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ListAgedMissingMediaAssets(1h): %v", err)
	}
	if len(got) != 1 || got[0].ID != assetID || got[0].RecordingID != id || got[0].Kind != "original" {
		t.Fatalf("ListAgedMissingMediaAssets(1h) = %+v, want only asset %d", got, assetID)
	}
	if got, err := f.Storage.ListAgedMissingMediaAssets(ctx, 3*time.Hour); err != nil || len(got) != 0 {
		t.Fatalf("ListAgedMissingMediaAssets(3h) = (%+v, %v), want none", got, err)
	}
	f.SetAssetState(t, assetID, "deleting")
	if got, err := f.Storage.ListAgedMissingMediaAssets(ctx, time.Hour); err != nil || len(got) != 0 {
		t.Fatalf("ListAgedMissingMediaAssets(1h) after the asset left active = (%+v, %v), want none", got, err)
	}
}

func runMediaEncodePlanningContract(t *testing.T, f MediaFixture) {
	t.Helper()
	ctx := context.Background()
	id := f.NewRecording(t)
	profile := "cut"
	assetID := f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/cut.mp4", id), "deleted")
	asset, err := f.Reconcile.GetEncodedAssetForPlan(ctx, id, profile)
	if err != nil {
		t.Fatalf("GetEncodedAssetForPlan(%d, %q): %v", id, profile, err)
	}
	if asset.ID != assetID || asset.State != "deleted" || asset.RelPath != fmt.Sprintf("sites/home/%d/cut.mp4", id) {
		t.Fatalf("encoded asset observation = %+v, want id %d with deleted state and its path", asset, assetID)
	}
	if len(asset.KeepRanges) != 0 {
		t.Fatalf("keep ranges before freezing = %v, want none", asset.KeepRanges)
	}
	if _, err := f.Reconcile.GetEncodedAssetForPlan(ctx, id, "missing-profile"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetEncodedAssetForPlan(unknown profile) error = %v, want ErrNotFound", err)
	}

	f.SetAssetState(t, assetID, "active")
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	f.SetMediaAssetKeepRanges(t, assetID, keep)
	frozen, err := f.Reconcile.GetEncodedAssetForPlan(ctx, id, profile)
	if err != nil {
		t.Fatalf("GetEncodedAssetForPlan(%d, %q) after freezing: %v", id, profile, err)
	}
	if frozen.State != "active" || !chapters.SameRanges(frozen.KeepRanges, keep) {
		t.Fatalf("observation after freezing = %+v, want active with keep ranges %v in the same read", frozen, keep)
	}

	matches, err := f.Reconcile.EncodedAssetRebuildRequestMatches(ctx, id, profile, keep)
	if err != nil || matches {
		t.Fatalf("EncodedAssetRebuildRequestMatches without a request = (%v, %v), want (false, nil)", matches, err)
	}
	f.SetEncodedAssetRebuildRequest(t, id, profile, keep)
	if matches, err := f.Reconcile.EncodedAssetRebuildRequestMatches(ctx, id, profile, keep); err != nil || !matches {
		t.Fatalf("EncodedAssetRebuildRequestMatches matching keep = (%v, %v), want (true, nil)", matches, err)
	}
	changedKeep := []chapters.Range{{StartMs: 0, EndMs: 900}}
	if matches, err := f.Reconcile.EncodedAssetRebuildRequestMatches(ctx, id, profile, changedKeep); err != nil || matches {
		t.Fatalf("EncodedAssetRebuildRequestMatches changed keep = (%v, %v), want (false, nil)", matches, err)
	}
}
