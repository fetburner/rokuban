package contracttest

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// TSScanSnapshot は計測結果と、それらが対応する原本サイズを表す。
// ScannedSizeBytes が -1 の場合、計測 marker は無い。
type TSScanSnapshot struct {
	Stats            []store.RecordingDropStat
	Positions        []store.RecordingDropPosition
	ScannedSizeBytes int64
}

// TSScanFixture は TS scan 契約と、その状態を作るための実装別フックをまとめる。
type TSScanFixture struct {
	Store                   store.TSScan
	Catalog                 store.Catalog
	NewOriginal             func(t *testing.T, sizeBytes int64, trashed bool) (recordingID, assetID int64)
	NewEncodedOnlyRecording func(t *testing.T) (recordingID, assetID int64)
	MarkOriginalMissing     func(t *testing.T, assetID int64)
	SetAssetSize            func(t *testing.T, assetID, sizeBytes int64)
	SetAssetState           func(t *testing.T, assetID int64, state string)
	SetScannedSize          func(t *testing.T, assetID, scannedSizeBytes int64)
	SeedSnapshot            func(t *testing.T, assetID int64, snapshot TSScanSnapshot)
	BeginAssetStateChange   func(t *testing.T, assetID int64, state string) func()
	WaitForLockWaiter       func(t *testing.T, operation string)
	// InPlace と OtherStoreInPlace は、同じ置き場と別の置き場から既存ファイルを登録する。
	InPlace                 InPlaceRegistrar
	OtherStoreInPlace       InPlaceRegistrar
	NewInPlaceOriginalInput func(t *testing.T, relPath string) store.InPlaceInput
	SetAssetSHA256          func(t *testing.T, assetID int64)
}

// NewTSScanStore は TS scan 契約の実装とフィクスチャを作る。
type NewTSScanStore func(t *testing.T) TSScanFixture

// RunTSScan は TS scan の読み取りと原子的な置き換えの契約を実行する。
func RunTSScan(t *testing.T, newStore NewTSScanStore) {
	t.Helper()
	t.Run("reads only active originals missing a current-size scan", func(t *testing.T) {
		runTSScanReads(t, newStore)
	})
	t.Run("replacement rechecks physical presence after reading the source", func(t *testing.T) {
		runTSScanReplacementRequiresPresentOriginal(t, newStore)
	})
	t.Run("replaces statistics and marker together", func(t *testing.T) {
		runTSScanReplacement(t, newStore)
	})
	t.Run("rechecks active state after waiting for the asset lock", func(t *testing.T) {
		runTSScanReplacementAfterStateChange(t, newStore)
	})
	t.Run("does not replace scans for encoded assets", func(t *testing.T) {
		runTSScanReplacementRejectsEncodedAsset(t, newStore)
	})
	t.Run("rolls back statistics and marker after a failed replacement", func(t *testing.T) {
		runTSScanReplacementRollback(t, newStore)
	})
	t.Run("in-place registration of an original discards its scan only when the content changes", func(t *testing.T) {
		runTSScanInPlaceInvalidation(t, newStore)
	})
	t.Run("in-place registration during a scan rejects the stale replacement", func(t *testing.T) {
		runTSScanInPlaceReplacementInFlight(t, newStore)
	})
	t.Run("catalog restore during a scan rejects the stale replacement", func(t *testing.T) {
		runTSScanCatalogRestoreInFlight(t, newStore)
	})
}

func runTSScanReads(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()

	currentID, currentAssetID := f.NewOriginal(t, 8, false)
	asset, err := f.Store.GetActiveOriginalForTSScan(ctx, currentID)
	if err != nil {
		t.Fatalf("GetActiveOriginalForTSScan(unmeasured) error = %v", err)
	}
	if asset.ID != currentAssetID || asset.SizeBytes != 8 {
		t.Fatalf("active original = %+v, want asset %d size 8", asset, currentAssetID)
	}
	f.SetScannedSize(t, currentAssetID, 8)
	if _, err := f.Store.GetActiveOriginalForTSScan(ctx, currentID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveOriginalForTSScan(current marker) error = %v, want ErrNotFound", err)
	}

	staleID, staleAssetID := f.NewOriginal(t, 9, false)
	f.SetScannedSize(t, staleAssetID, 8)
	trashedID, _ := f.NewOriginal(t, 10, true)

	got, err := f.Store.ListMissingTSScanRecordings(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListMissingTSScanRecordings: %v", err)
	}
	if !slices.Contains(got, staleID) {
		t.Errorf("missing TS scan recordings = %v, want stale recording %d", got, staleID)
	}
	if slices.Contains(got, currentID) {
		t.Errorf("missing TS scan recordings = %v, current-size recording %d should be excluded", got, currentID)
	}
	if slices.Contains(got, trashedID) {
		t.Errorf("missing TS scan recordings = %v, trashed recording %d should be excluded", got, trashedID)
	}

	deletingID, deletingAssetID := f.NewOriginal(t, 11, false)
	f.SetAssetState(t, deletingAssetID, "deleting")
	if _, err := f.Store.GetActiveOriginalForTSScan(ctx, deletingID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetActiveOriginalForTSScan(deleting original) error = %v, want ErrNotFound", err)
	}

	encodedOnlyID, _ := f.NewEncodedOnlyRecording(t)
	if _, err := f.Store.GetActiveOriginalForTSScan(ctx, encodedOnlyID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetActiveOriginalForTSScan(encoded-only recording) error = %v, want ErrNotFound", err)
	}

	got, err = f.Store.ListMissingTSScanRecordings(ctx, 0, 20)
	if err != nil {
		t.Fatalf("ListMissingTSScanRecordings after adding excluded assets: %v", err)
	}
	if slices.Contains(got, deletingID) {
		t.Errorf("missing TS scan recordings = %v, deleting original %d should be excluded", got, deletingID)
	}
	if slices.Contains(got, encodedOnlyID) {
		t.Errorf("missing TS scan recordings = %v, encoded-only recording %d should be excluded", got, encodedOnlyID)
	}

	missingID, missingAssetID := f.NewOriginal(t, 12, false)
	f.MarkOriginalMissing(t, missingAssetID)
	if _, err := f.Store.GetActiveOriginalForTSScan(ctx, missingID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetActiveOriginalForTSScan(missing copy) = %v, want ErrNotFound", err)
	}
	if _, err := f.Store.GetActiveOriginalForTSScan(ctx, staleID); err != nil {
		t.Errorf("GetActiveOriginalForTSScan(present unmeasured copy) = %v, want found", err)
	}
	got, err = f.Store.ListMissingTSScanRecordings(ctx, 0, 20)
	if err != nil || !slices.Contains(got, staleID) || slices.Contains(got, missingID) {
		t.Errorf("TS scan candidates = %v, %v; want present %d included and missing %d excluded", got, err, staleID, missingID)
	}
}

func runTSScanReplacementRejectsEncodedAsset(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	_, assetID := f.NewEncodedOnlyRecording(t)
	source := store.TSScanSource{MediaAsset: store.MediaAsset{ID: assetID}}

	applied, err := f.Store.ReplaceScan(ctx, source, 1,
		[]store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1}},
		[]store.RecordingDropPosition{{Pid: 256, ByteOffset: 188}},
	)
	if err != nil {
		t.Fatalf("ReplaceScan for encoded asset: %v", err)
	}
	if applied {
		t.Fatal("ReplaceScan for encoded asset = true, want no replacement")
	}
	document, err := f.Catalog.Export(ctx)
	if err != nil {
		t.Fatalf("Catalog.Export after rejected encoded-asset ReplaceScan: %v", err)
	}
	got := readTSScanSnapshotForAsset(document, assetID)
	if len(got.Stats) != 0 || len(got.Positions) != 0 || got.ScannedSizeBytes != -1 {
		t.Fatalf("Catalog.Export for encoded asset %d after rejected ReplaceScan: len(DropStats) = %d, len(DropPositions) = %d, ScannedSizeBytes = %d, want 0, 0, -1 (no scan marker)", assetID, len(got.Stats), len(got.Positions), got.ScannedSizeBytes)
	}
}

func runTSScanReplacement(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID, assetID := f.NewOriginal(t, 6, false)
	source, sourceErr := f.Store.GetActiveOriginalForTSScan(ctx, recordingID)
	if sourceErr != nil {
		t.Fatal(sourceErr)
	}

	oldStats := []store.RecordingDropStat{{Pid: 700, Packets: 9, Drops: 2, Errors: 1, Scrambled: 3}}
	oldPositions := []store.RecordingDropPosition{{Pid: 700, ByteOffset: 8}}
	f.SeedSnapshot(t, assetID, TSScanSnapshot{Stats: oldStats, Positions: oldPositions, ScannedSizeBytes: 4})

	pidType := "video"
	elapsed := int64(1234)
	wantStats := []store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1, PidType: &pidType}}
	wantPositions := []store.RecordingDropPosition{{Pid: 256, ByteOffset: 188, ElapsedMs: &elapsed}}
	applied, err := f.Store.ReplaceScan(ctx, source, 6, wantStats, wantPositions)
	if err != nil {
		t.Fatalf("ReplaceScan: %v", err)
	}
	if !applied {
		t.Fatal("ReplaceScan() = false, want replacement")
	}
	requireTSScanSnapshot(t, f, recordingID, TSScanSnapshot{Stats: wantStats, Positions: wantPositions})
	assertCurrentTSScanMarker(t, f, recordingID, 6)

	f.SetAssetSize(t, assetID, 7)
	updated, err := f.Store.ReplaceScan(ctx, source, 6, wantStats, wantPositions)
	if err != nil {
		t.Fatalf("ReplaceScan after original size changed: %v", err)
	}
	if updated {
		t.Fatal("ReplaceScan after original size changed = true, want no replacement")
	}
	requireTSScanSnapshot(t, f, recordingID, TSScanSnapshot{Stats: wantStats, Positions: wantPositions})
	assertStaleTSScanMarker(t, f, recordingID, assetID, 7, 6)
}

func runTSScanReplacementAfterStateChange(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID, assetID := f.NewOriginal(t, 6, false)
	source, sourceErr := f.Store.GetActiveOriginalForTSScan(ctx, recordingID)
	if sourceErr != nil {
		t.Fatal(sourceErr)
	}
	before := TSScanSnapshot{
		Stats:            []store.RecordingDropStat{{Pid: 700, Packets: 9, Drops: 2, Errors: 1, Scrambled: 3}},
		Positions:        []store.RecordingDropPosition{{Pid: 700, ByteOffset: 8}},
		ScannedSizeBytes: 4,
	}
	f.SeedSnapshot(t, assetID, before)

	finishStateChange := f.BeginAssetStateChange(t, assetID, "deleting")
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(finishStateChange) }
	t.Cleanup(finish)

	type replaceResult struct {
		applied bool
		err     error
	}
	resultCh := make(chan replaceResult, 1)
	go func() {
		applied, err := f.Store.ReplaceScan(ctx, source, 6,
			[]store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1}},
			[]store.RecordingDropPosition{{Pid: 256, ByteOffset: 188}},
		)
		resultCh <- replaceResult{applied: applied, err: err}
	}()
	f.WaitForLockWaiter(t, "ReplaceScan after asset state changed")
	finish()

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("ReplaceScan after asset state changed: %v", result.err)
		}
		if result.applied {
			t.Fatal("ReplaceScan after asset state changed = true, want no replacement")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReplaceScan did not finish after the asset state change committed")
	}
	// ブロッカーが asset を非 active にしたままなので、active へ戻して
	// GetActiveOriginalForTSScan が古い印を観測できるようにする。
	f.SetAssetState(t, assetID, "active")
	requireTSScanSnapshot(t, f, recordingID, before)
	assertStaleTSScanMarker(t, f, recordingID, assetID, 6, before.ScannedSizeBytes)
}

func runTSScanReplacementRollback(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID, assetID := f.NewOriginal(t, 6, false)
	source, sourceErr := f.Store.GetActiveOriginalForTSScan(ctx, recordingID)
	if sourceErr != nil {
		t.Fatal(sourceErr)
	}

	oldStats := []store.RecordingDropStat{{Pid: 700, Packets: 9, Drops: 2, Errors: 1, Scrambled: 3}}
	oldPositions := []store.RecordingDropPosition{{Pid: 700, ByteOffset: 8}}
	before := TSScanSnapshot{Stats: oldStats, Positions: oldPositions, ScannedSizeBytes: 4}
	f.SeedSnapshot(t, assetID, before)

	pidType := "video"
	elapsed := int64(1234)
	_, err := f.Store.ReplaceScan(ctx, source, 6,
		[]store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1, PidType: &pidType}},
		[]store.RecordingDropPosition{
			{Pid: 256, ByteOffset: 188, ElapsedMs: &elapsed},
			{Pid: 256, ByteOffset: 188, ElapsedMs: &elapsed},
		},
	)
	if err == nil {
		t.Fatal("ReplaceScan() succeeded with a failing drop_positions insert, want error")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReplaceScan() error = %v, want store.ErrConflict", err)
	}
	requireTSScanSnapshot(t, f, recordingID, before)
	assertStaleTSScanMarker(t, f, recordingID, assetID, 6, before.ScannedSizeBytes)
}

func readTSScanSnapshot(t *testing.T, f TSScanFixture, recordingID int64) TSScanSnapshot {
	t.Helper()
	document, err := f.Catalog.Export(context.Background())
	if err != nil {
		t.Fatalf("Catalog.Export while reading TS scan state: %v", err)
	}
	for _, asset := range document.MediaAssets {
		if asset.RecordingID == recordingID && asset.Kind == store.AssetKindOriginal {
			return readTSScanSnapshotForAsset(document, asset.ID)
		}
	}
	return TSScanSnapshot{ScannedSizeBytes: -1}
}

func readTSScanSnapshotForAsset(document *catalogdoc.Document, assetID int64) TSScanSnapshot {
	got := TSScanSnapshot{ScannedSizeBytes: -1}
	for _, stat := range document.DropStats {
		if stat.MediaAssetID == assetID {
			got.Stats = append(got.Stats, store.RecordingDropStat{
				Pid: stat.Pid, Packets: stat.Packets, Drops: stat.Drops, Errors: stat.Errors,
				Scrambled: stat.Scrambled, PidType: stat.PidType,
			})
		}
	}
	for _, position := range document.DropPositions {
		if position.MediaAssetID == assetID {
			got.Positions = append(got.Positions, store.RecordingDropPosition{
				Pid: position.Pid, ByteOffset: position.ByteOffset, ElapsedMs: position.ElapsedMs,
			})
		}
	}
	for _, scan := range document.MediaAssetTSScans {
		if scan.MediaAssetID == assetID {
			got.ScannedSizeBytes = scan.ScannedSizeBytes
			break
		}
	}
	return got
}

func requireTSScanSnapshot(t *testing.T, f TSScanFixture, recordingID int64, want TSScanSnapshot) {
	t.Helper()
	got := readTSScanSnapshot(t, f, recordingID)
	reportCatalogDiff(t, "Catalog.Export.DropStats for recording "+strconv.FormatInt(recordingID, 10), reflect.ValueOf(got.Stats), reflect.ValueOf(want.Stats))
	reportCatalogDiff(t, "Catalog.Export.DropPositions for recording "+strconv.FormatInt(recordingID, 10), reflect.ValueOf(got.Positions), reflect.ValueOf(want.Positions))
	if t.Failed() {
		t.FailNow()
	}
}

func assertCurrentTSScanMarker(t *testing.T, f TSScanFixture, recordingID, scannedSizeBytes int64) {
	t.Helper()
	if _, err := f.Store.GetActiveOriginalForTSScan(context.Background(), recordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveOriginalForTSScan(%d) after current-size scan = %v, want ErrNotFound", recordingID, err)
	}
	if got := readTSScanSnapshot(t, f, recordingID).ScannedSizeBytes; got != scannedSizeBytes {
		t.Fatalf("Catalog.Export.MediaAssetTSScans scanned size for recording %d = %d, want %d", recordingID, got, scannedSizeBytes)
	}
}

func assertStaleTSScanMarker(t *testing.T, f TSScanFixture, recordingID, assetID, sizeBytes, scannedSizeBytes int64) {
	t.Helper()
	asset, err := f.Store.GetActiveOriginalForTSScan(context.Background(), recordingID)
	if err != nil || asset.ID != assetID || asset.SizeBytes != sizeBytes {
		t.Fatalf("GetActiveOriginalForTSScan(%d) with stale marker: ID = %d, SizeBytes = %d, err = %v, want ID %d, SizeBytes %d, nil error", recordingID, asset.ID, asset.SizeBytes, err, assetID, sizeBytes)
	}
	if got := readTSScanSnapshot(t, f, recordingID).ScannedSizeBytes; got != scannedSizeBytes {
		t.Fatalf("Catalog.Export.MediaAssetTSScans stale size for recording %d = %d, want %d", recordingID, got, scannedSizeBytes)
	}
}

func runTSScanReplacementRequiresPresentOriginal(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := t.Context()
	for _, missing := range []bool{false, true} {
		id, assetID := f.NewOriginal(t, 8, false)
		source, err := f.Store.GetActiveOriginalForTSScan(ctx, id)
		if err != nil {
			t.Fatalf("getting TS scan source: %v", err)
		}
		if missing {
			f.MarkOriginalMissing(t, assetID)
		}
		applied, err := f.Store.ReplaceScan(ctx, source, 8, nil, nil)
		if err != nil || applied == missing {
			t.Errorf("ReplaceScan after missing=%v: %v, %v; want applied=%v, nil", missing, applied, err, !missing)
		}
	}
}
