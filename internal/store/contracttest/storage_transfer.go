package contracttest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

// StorageTransferFixture は転送契約の実装と、状態作り・並行実行のフックをまとめる。
type StorageTransferFixture struct {
	Store                          store.StorageTransfer
	LoopMarkers                    store.LoopMarkers
	RejectTransferCompletionMarker func(t *testing.T) (restore func())
	WithPolicy                     func(storeName string, rules []store.OriginalStoreRule, fsStores []string, derivedStore string) store.StorageTransfer
	NewPendingCopy                 func(t *testing.T) (copyID, assetID int64)
	ExpireProgressLease            func(t *testing.T, copyID int64)
	DeleteCopy                     func(t *testing.T, copyID int64)
	BeginCopyRetirement            func(t *testing.T, copyID int64) (commit func())
	BeginBacklogWriteHold          func(t *testing.T) (end func())
	WaitForBlockedTransfers        func(t *testing.T, count int, finished <-chan struct{})
	NewRecording                   func(t *testing.T, site string, programStartAt time.Time) int64
	TrashRecording                 func(t *testing.T, recordingID int64)
	NewAsset                       func(t *testing.T, recordingID int64, kind, sourceStore, pathPrefix string) (assetID, copyID int64)
	AddCopy                        func(t *testing.T, assetID int64, storeName, relPath, state string, generation int64) int64
	MarkCopyMissing                func(t *testing.T, copyID int64)
	SeedTransferAttempts           func(t *testing.T, copyID int64, count int32, message string)
}

// NewStorageTransferStore は空の DB に転送契約の実装とフィクスチャを作る。
type NewStorageTransferStore func(t *testing.T) StorageTransferFixture

// RunStorageTransfer は転送、転送元の排他、背圧スナップショットの契約を実行する。
func RunStorageTransfer(t *testing.T, newStore NewStorageTransferStore) {
	t.Helper()
	for _, tc := range []struct {
		name string
		run  func(*testing.T, NewStorageTransferStore)
	}{
		{"TransferProgressFreshRowStartsGenerationOne", runTransferProgressFreshRowStartsGenerationOne},
		{"TransferProgressLiveLeaseBlocksOtherOwner", runTransferProgressLiveLeaseBlocksOtherOwner},
		{"TransferProgressCleanHandoffResumesSavedState", runTransferProgressCleanHandoffResumesSavedState},
		{"TransferProgressExpiredLeaseStartsNewGeneration", runTransferProgressExpiredLeaseStartsNewGeneration},
		{"TransferProgressZombieSaveIsRejected", runTransferProgressZombieSaveIsRejected},
		{"TransferProgressRowIsDeletedWithPendingCopy", runTransferProgressRowIsDeletedWithPendingCopy},
		{"TransferProgressSaveAfterOwnLeaseExpiryIsRejected", runTransferProgressSaveAfterOwnLeaseExpiryIsRejected},
		{"TransferProgressAcquireWaitsForConcurrentCopyRetirement", runTransferProgressAcquireWaitsForConcurrentCopyRetirement},
		{"TransferUploadReplaceAbandonsPreviousState", runTransferUploadReplaceAbandonsPreviousState},
		{"TransferUploadReplaceRequiresLiveLease", runTransferUploadReplaceRequiresLiveLease},
		{"TransferUploadSavedStateReturnsOnlyOnCleanHandoff", runTransferUploadSavedStateReturnsOnlyOnCleanHandoff},
		{"TransferUploadRowDeletionAbandonsState", runTransferUploadRowDeletionAbandonsState},
		{"TransferUploadAbandonedListFiltersByStoreInIDOrder", runTransferUploadAbandonedListFiltersByStoreInIDOrder},
		{"TransferSourceVerificationRequiresLiveLease", runTransferSourceVerificationRequiresLiveLease},
		{"TransferSourceVerificationSurvivesHandoffs", runTransferSourceVerificationSurvivesHandoffs},
		{"RecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration", runRecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration},
		{"RecordTransferBacklog_ReturnsPreviousAndDropsOthers", runRecordTransferBacklog_ReturnsPreviousAndDropsOthers},
		{"RecordTransferBacklog_StaleCountDoesNotOverwrite", runRecordTransferBacklog_StaleCountDoesNotOverwrite},
		{"RecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce", runRecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce},
		{"CountMediaAssetTransferBacklogBySourceStoreMatchesCandidates", runCountMediaAssetTransferBacklogBySourceStoreMatchesCandidates},
		{"StorageTransferDestination", runStorageTransferDestination},
		{"StorageTransferPendingCopyObsolete", runStorageTransferPendingCopyObsolete},
		{"TransferBandwidthLease", runTransferBandwidthLease},
		{"TransferProgressRelease", runTransferProgressRelease},
		{"TransferCompletionAndOwnership", runTransferCompletionAndOwnership},
		{"TransferFailureAndObsoleteAttempts", runTransferFailureAndObsoleteAttempts},
		{"ReclaimObsoletePendingTransferCopy", runReclaimObsoletePendingTransferCopy},
		{"DeleteStaleTransferCopy", runDeleteStaleTransferCopy},
		{"MarkCorruptDerivedCopy", runMarkCorruptDerivedCopy},
		{"ListOldRemoteDerivedCopies", runListOldRemoteDerivedCopies},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newStore) })
	}
}

func runTransferBandwidthLease(t *testing.T, newStore NewStorageTransferStore) {
	media := newStore(t).Store
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	lease, acquired, err := media.AcquireTransferSourceBandwidth(ctx, "test-store")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("first AcquireTransferSourceBandwidth = (%v, %t, %v), want a lease", lease, acquired, err)
	}
	released := false
	defer func() {
		if !released {
			if err := lease.Release(ctx); err != nil {
				t.Errorf("releasing first lease: %v", err)
			}
		}
	}()
	blocked, acquired, err := media.AcquireTransferSourceBandwidth(ctx, "test-store")
	if acquired && blocked != nil {
		defer func() { _ = blocked.Release(ctx) }()
	}
	if err != nil || acquired || blocked != nil {
		t.Fatalf("second AcquireTransferSourceBandwidth = (%v, %t, %v), want busy", blocked, acquired, err)
	}
	other, acquired, err := media.AcquireTransferSourceBandwidth(ctx, "other-store")
	if err != nil || !acquired || other == nil {
		t.Fatalf("AcquireTransferSourceBandwidth for other-store = (%v, %t, %v), want a lease while test-store is held", other, acquired, err)
	}
	defer func() {
		if err := other.Release(ctx); err != nil {
			t.Errorf("releasing other-store lease: %v", err)
		}
	}()
	released = true
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	next, acquired, err := media.AcquireTransferSourceBandwidth(ctx, "test-store")
	if err != nil || !acquired || next == nil {
		t.Fatalf("AcquireTransferSourceBandwidth after Release = (%v, %t, %v), want a lease", next, acquired, err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatalf("releasing reacquired lease: %v", err)
	}
}

func runTransferProgressRelease(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	ctx := t.Context()
	copyID, _ := f.NewPendingCopy(t)
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	if err := media.ReleaseMediaAssetTransferProgress(ctx, copyID, uuid.New()); err != nil {
		t.Fatalf("Release by a non-owner: %v", err)
	}
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after non-owner Release = (%t, %v), want busy", acquired, err)
	}
	if err := media.ReleaseMediaAssetTransferProgress(ctx, copyID, owner); err != nil {
		t.Fatalf("Release by owner: %v", err)
	}
	got := acquireLive(t, media, copyID, uuid.New())
	if got.WriterGeneration != 2 || got.PrevWriterGeneration != 1 || got.Resumed || got.Offset != 0 || got.HashState != nil {
		t.Fatalf("Acquire after Release = %+v, want fresh generation 2", got)
	}
	if generation, ok, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, copyID); err != nil || !ok || generation != 2 {
		t.Fatalf("GetMediaAssetTransferProgressWriterGeneration = (%d, %t, %v), want generation 2", generation, ok, err)
	}
}

// コピーを pending でなくす transaction と並行に進捗を取ると、退役を観測し、行を作らない。
// 待たずに古い状態で作ると、pending でないコピーの世代の temp が GC で消えなくなる。
func runTransferProgressAcquireWaitsForConcurrentCopyRetirement(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	copyID, _ := f.NewPendingCopy(t)
	commit := f.BeginCopyRetirement(t, copyID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	var acquired bool
	var err error
	go func() {
		defer close(done)
		_, acquired, err = f.Store.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute)
	}()
	f.WaitForBlockedTransfers(t, 1, done)
	commit()
	select {
	case <-done:
		if err != nil || acquired {
			t.Fatalf("Acquire on a copy retired concurrently = (acquired=%t, %v), want not acquired", acquired, err)
		}
	case <-ctx.Done():
		t.Fatal("Acquire did not finish after retirement committed")
	}
	if _, ok, err := f.Store.GetMediaAssetTransferProgressWriterGeneration(ctx, copyID); err != nil || ok {
		t.Fatalf("progress after concurrent retirement = (ok=%t, %v), want no row", ok, err)
	}
}
