package contracttest

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

func runTransferCompletionAndOwnership(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	fsStores := []string{"edge", "backup", "archive"}
	media := f.WithPolicy("archive", transferOriginalRules, fsStores, "derived")
	recordingID := f.NewRecording(t, "tiered", time.Time{})
	assetID, sourceCopyID := f.NewAsset(t, recordingID, "encoded", "edge", "publish")
	if _, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "archive", fsStores, fsStores, fsStores, 10); err != nil || ok {
		t.Fatalf("Prepare to undesired archive = (%t, %v), want false", ok, err)
	}

	transfer, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "derived", fsStores, fsStores, fsStores, 10)
	if err != nil || !ok || transfer.MediaAssetID != assetID || transfer.RecordingID != recordingID ||
		transfer.DesiredStore != "derived" || transfer.SourceStore != "edge" || transfer.SourceCopyID != sourceCopyID ||
		transfer.SourceRelPath == "" || transfer.CopyID == 0 || transfer.RelPath == "" ||
		transfer.SizeBytes != 1 || transfer.Kind != "encoded" || transfer.Generation != 0 {
		t.Fatalf("Prepare = (%+v, %t, %v), want a pending transfer from edge to derived", transfer, ok, err)
	}
	again, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "derived", fsStores, fsStores, fsStores, 10)
	if err != nil || !ok || again.CopyID != transfer.CopyID || again.RelPath != transfer.RelPath {
		t.Fatalf("repeated Prepare = (%+v, %t, %v), want the existing copy at %q", again, ok, err, transfer.RelPath)
	}
	acquireLive(t, media, transfer.CopyID, uuid.New())
	for _, path := range []string{transfer.RelPath, "publish/subtitle.vtt"} {
		if got, err := media.MarkMediaAssetTransferObjectOwned(ctx, transfer.CopyID, "derived", path); err != nil || got != store.TransferObjectOwned {
			t.Fatalf("MarkMediaAssetTransferObjectOwned(%q) = (%v, %v), want owned", path, got, err)
		}
	}
	if paths, err := media.ListMediaAssetTransferObjectsOwned(ctx, transfer.CopyID, "derived"); err != nil ||
		len(paths) != 2 || !slices.Contains(paths, transfer.RelPath) || !slices.Contains(paths, "publish/subtitle.vtt") {
		t.Fatalf("owned keys = (%q, %v), want main and subtitle", paths, err)
	}
	if err := media.DeleteMediaAssetTransferObjectOwned(ctx, transfer.CopyID, "derived", "publish/subtitle.vtt"); err != nil {
		t.Fatalf("DeleteMediaAssetTransferObjectOwned: %v", err)
	}
	if paths, err := media.ListMediaAssetTransferObjectsOwned(ctx, transfer.CopyID, "derived"); err != nil || !slices.Equal(paths, []string{transfer.RelPath}) {
		t.Fatalf("owned keys after deleting subtitle = (%q, %v), want only main", paths, err)
	}
	sum := []byte("0123456789abcdef0123456789abcdef")
	if recorded, err := media.RecordObservedMediaAssetSHA256(ctx, assetID, 0, sum); err != nil || !recorded {
		t.Fatalf("RecordObservedMediaAssetSHA256 = (%t, %v), want recorded", recorded, err)
	}
	other := []byte("fedcba9876543210fedcba9876543210")
	if active, reason, err := media.CompleteMediaAssetTransfer(ctx, transfer, "derived", other); err != nil || active || reason != "sha_mismatch" {
		t.Fatalf("Complete with wrong digest = (%t, %q, %v), want sha_mismatch", active, reason, err)
	}

	restoreMarker := f.RejectTransferCompletionMarker(t)
	if active, _, err := media.CompleteMediaAssetTransfer(ctx, transfer, "derived", sum); err == nil || active {
		t.Fatalf("Complete with marker write failure = (active=%t, %v), want error and no activation", active, err)
	}
	restoreMarker()
	job := store.MediaAssetCopyDeleteCandidate{CopyID: transfer.CopyID, MediaAssetID: assetID, Store: "derived"}
	if copy, found, err := media.GetMediaAssetCopyForDeletion(ctx, job); err != nil || !found || copy.State != "pending" {
		t.Fatalf("copy after marker failure = (%+v, %t, %v), want pending", copy, found, err)
	}
	if gen, found, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, transfer.CopyID); err != nil || !found || gen != 1 {
		t.Fatalf("progress after marker failure = (%d, %t, %v), want generation 1", gen, found, err)
	}
	if paths, err := media.ListMediaAssetTransferObjectsOwned(ctx, transfer.CopyID, "derived"); err != nil || !slices.Equal(paths, []string{transfer.RelPath}) {
		t.Fatalf("owned keys after marker failure = (%q, %v), want main", paths, err)
	}

	before := transferDatabaseClock(t, media)
	if active, reason, err := media.CompleteMediaAssetTransfer(ctx, transfer, "derived", sum); err != nil || !active || reason != "activated" {
		t.Fatalf("Complete = (%t, %q, %v), want activated", active, reason, err)
	}
	after := transferDatabaseClock(t, media)
	markers, err := f.LoopMarkers.ReadLoopMarkers(ctx, "storage_transfer")
	at := markers["all"]
	if err != nil || len(markers) != 1 || at.IsZero() || at.Before(before) || at.After(after) {
		t.Fatalf("completion markers = (%v, %v), want all in [%v, %v]", markers, err, before, after)
	}
	if active, reason, err := media.CompleteMediaAssetTransfer(ctx, transfer, "derived", sum); err != nil || !active || reason != "already_active" {
		t.Fatalf("repeated Complete = (%t, %q, %v), want already_active", active, reason, err)
	}
	if _, found, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, transfer.CopyID); err != nil || found {
		t.Fatalf("progress after completion = (%t, %v), want none", found, err)
	}
	if paths, err := media.ListMediaAssetTransferObjectsOwned(ctx, transfer.CopyID, "derived"); err != nil || len(paths) != 0 {
		t.Fatalf("owned keys after completion = (%q, %v), want none", paths, err)
	}
	if got, err := media.MarkMediaAssetTransferObjectOwned(ctx, transfer.CopyID, "derived", transfer.RelPath); err != nil || got != store.TransferObjectCopyAdvanced {
		t.Fatalf("marking ownership after completion = (%v, %v), want advanced", got, err)
	}

	retiredCopyID, _ := f.NewPendingCopy(t)
	if err := media.DiscardPendingMediaAssetCopy(ctx, retiredCopyID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got, err := media.MarkMediaAssetTransferObjectOwned(ctx, retiredCopyID, "archive", "publish/retired.ts"); err != nil || got != store.TransferObjectCopyRetired {
		t.Fatalf("marking ownership after discard = (%v, %v), want retired", got, err)
	}
}

// runOwnedKeysCompareByBytes は、見た目が同じで UTF-8 のバイトが違う key を別々に所有することを確かめる。
// MySQL の rel_path は接続の照合順序で比べると同じ扱いになりうるので、比較はバイト単位（SHA-256）で行う。
func runOwnedKeysCompareByBytes(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	copyID, _ := f.NewPendingCopy(t)
	acquireLive(t, f.Store, copyID, uuid.New())
	composed := "publish/caf\u00e9.vtt"
	decomposed := "publish/cafe\u0301.vtt"
	for _, path := range []string{composed, decomposed} {
		if got, err := f.Store.MarkMediaAssetTransferObjectOwned(ctx, copyID, "archive", path); err != nil || got != store.TransferObjectOwned {
			t.Fatalf("MarkMediaAssetTransferObjectOwned(%q) = (%v, %v), want owned", path, got, err)
		}
	}
	paths, err := f.Store.ListMediaAssetTransferObjectsOwned(ctx, copyID, "archive")
	if err != nil || len(paths) != 2 {
		t.Fatalf("owned keys = (%q, %v), want both spellings", paths, err)
	}
	// 削除も同じバイト比較で行う。composed を消しても decomposed の記録は残る。
	if err := f.Store.DeleteMediaAssetTransferObjectOwned(ctx, copyID, "archive", composed); err != nil {
		t.Fatalf("DeleteMediaAssetTransferObjectOwned(%q): %v", composed, err)
	}
	paths, err = f.Store.ListMediaAssetTransferObjectsOwned(ctx, copyID, "archive")
	if err != nil || !slices.Equal(paths, []string{decomposed}) {
		t.Fatalf("owned keys after deleting composed = (%q, %v), want only decomposed", paths, err)
	}
}
