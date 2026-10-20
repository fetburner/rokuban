package contracttest

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

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
	NewOriginal             func(t *testing.T, sizeBytes int64, trashed bool) (recordingID, assetID int64)
	NewEncodedOnlyRecording func(t *testing.T) (recordingID, assetID int64)
	SetAssetSize            func(t *testing.T, assetID, sizeBytes int64)
	SetAssetState           func(t *testing.T, assetID int64, state string)
	SetScannedSize          func(t *testing.T, assetID, scannedSizeBytes int64)
	SeedSnapshot            func(t *testing.T, assetID int64, snapshot TSScanSnapshot)
	ReadSnapshot            func(t *testing.T, assetID int64) TSScanSnapshot
	BeginAssetStateChange   func(t *testing.T, assetID int64, state string) func()
	WaitForLockWaiter       func(t *testing.T, operation string)
}

// NewTSScanStore は TS scan 契約の実装とフィクスチャを作る。
type NewTSScanStore func(t *testing.T) TSScanFixture

// RunTSScan は TS scan の読み取りと原子的な置き換えの契約を実行する。
func RunTSScan(t *testing.T, newStore NewTSScanStore) {
	t.Helper()
	t.Run("reads only active originals missing a current-size scan", func(t *testing.T) {
		runTSScanReads(t, newStore)
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
}

func runTSScanReplacementRejectsEncodedAsset(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	_, assetID := f.NewEncodedOnlyRecording(t)
	before := TSScanSnapshot{Stats: []store.RecordingDropStat{}, Positions: []store.RecordingDropPosition{}, ScannedSizeBytes: -1}

	applied, err := f.Store.ReplaceScan(ctx, assetID, 1,
		[]store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1}},
		[]store.RecordingDropPosition{{Pid: 256, ByteOffset: 188}},
	)
	if err != nil {
		t.Fatalf("ReplaceScan for encoded asset: %v", err)
	}
	if applied {
		t.Fatal("ReplaceScan for encoded asset = true, want no replacement")
	}
	if got := f.ReadSnapshot(t, assetID); !reflect.DeepEqual(got, before) {
		t.Fatalf("snapshot after rejected encoded-asset ReplaceScan = %+v, want unchanged %+v", got, before)
	}
}

func runTSScanReplacement(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	_, assetID := f.NewOriginal(t, 6, false)

	oldStats := []store.RecordingDropStat{{Pid: 700, Packets: 9, Drops: 2, Errors: 1, Scrambled: 3}}
	oldPositions := []store.RecordingDropPosition{{Pid: 700, ByteOffset: 8}}
	f.SeedSnapshot(t, assetID, TSScanSnapshot{Stats: oldStats, Positions: oldPositions, ScannedSizeBytes: 4})

	pidType := "video"
	elapsed := int64(1234)
	wantStats := []store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1, PidType: &pidType}}
	wantPositions := []store.RecordingDropPosition{{Pid: 256, ByteOffset: 188, ElapsedMs: &elapsed}}
	applied, err := f.Store.ReplaceScan(ctx, assetID, 6, wantStats, wantPositions)
	if err != nil {
		t.Fatalf("ReplaceScan: %v", err)
	}
	if !applied {
		t.Fatal("ReplaceScan() = false, want replacement")
	}
	got := f.ReadSnapshot(t, assetID)
	want := TSScanSnapshot{Stats: wantStats, Positions: wantPositions, ScannedSizeBytes: 6}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot after ReplaceScan = %+v, want %+v", got, want)
	}

	f.SetAssetSize(t, assetID, 7)
	updated, err := f.Store.ReplaceScan(ctx, assetID, 6, wantStats, wantPositions)
	if err != nil {
		t.Fatalf("ReplaceScan after original size changed: %v", err)
	}
	if updated {
		t.Fatal("ReplaceScan after original size changed = true, want no replacement")
	}
	if got := f.ReadSnapshot(t, assetID); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot after rejected ReplaceScan = %+v, want prior rows and marker %+v", got, want)
	}
}

func runTSScanReplacementAfterStateChange(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	_, assetID := f.NewOriginal(t, 6, false)
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
		applied, err := f.Store.ReplaceScan(ctx, assetID, 6,
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
	if got := f.ReadSnapshot(t, assetID); !reflect.DeepEqual(got, before) {
		t.Fatalf("snapshot after rejected ReplaceScan = %+v, want prior rows and marker %+v", got, before)
	}
}

func runTSScanReplacementRollback(t *testing.T, newStore NewTSScanStore) {
	f := newStore(t)
	ctx := context.Background()
	_, assetID := f.NewOriginal(t, 6, false)

	oldStats := []store.RecordingDropStat{{Pid: 700, Packets: 9, Drops: 2, Errors: 1, Scrambled: 3}}
	oldPositions := []store.RecordingDropPosition{{Pid: 700, ByteOffset: 8}}
	before := TSScanSnapshot{Stats: oldStats, Positions: oldPositions, ScannedSizeBytes: 4}
	f.SeedSnapshot(t, assetID, before)

	pidType := "video"
	elapsed := int64(1234)
	_, err := f.Store.ReplaceScan(ctx, assetID, 6,
		[]store.RecordingDropStat{{Pid: 256, Packets: 3, Drops: 1, PidType: &pidType}},
		[]store.RecordingDropPosition{
			{Pid: 256, ByteOffset: 188, ElapsedMs: &elapsed},
			{Pid: 256, ByteOffset: 188, ElapsedMs: &elapsed},
		},
	)
	if err == nil {
		t.Fatal("ReplaceScan() succeeded with a failing drop_positions insert, want error")
	}
	if got := f.ReadSnapshot(t, assetID); !reflect.DeepEqual(got, before) {
		t.Fatalf("snapshot after failed ReplaceScan = %+v, want prior rows and marker %+v", got, before)
	}
}
