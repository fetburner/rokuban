package pgstore

import (
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestTransferSourceVerificationResetsStoredCheckpoint は照合の開始と終了がその場で保存値を初期化することを確かめる。
func TestTransferSourceVerificationResetsStoredCheckpoint(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := testMedia(pool, "test-store", "")
	copyID, _ := progressTestCopy(t, pool)
	ctx := t.Context()

	owner := acquireProgressTestLease(t, media, copyID)
	owner = saveAndReacquireProgressTestLease(t, media, copyID, owner, 5, []byte("h"), nil)
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	requireStoredTransferCheckpoint(t, pool, copyID, 0, nil)

	owner = saveAndReacquireProgressTestLease(t, media, copyID, owner, 5, []byte("h"), nil)
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); err != nil {
		t.Fatalf("End: %v", err)
	}
	requireStoredTransferCheckpoint(t, pool, copyID, 0, nil)
}

// TestTransferSourceVerificationExpiredLeaseKeepsStoredCheckpoint は期限切れで拒まれた開始と終了が保存値を保つことを確かめる。
func TestTransferSourceVerificationExpiredLeaseKeepsStoredCheckpoint(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := testMedia(pool, "test-store", "")
	copyID, _ := progressTestCopy(t, pool)
	ctx := t.Context()

	owner := acquireProgressTestLease(t, media, copyID)
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	owner = saveAndReacquireProgressTestLease(t, media, copyID, owner, 5, []byte("h"), nil)
	expireProgressLease(t, pool, copyID)

	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin after lease expiry = %v, want ErrTransferLeaseLost", err)
	}
	requireStoredTransferCheckpoint(t, pool, copyID, 5, []byte("h"))
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("End after lease expiry = %v, want ErrTransferLeaseLost", err)
	}
	requireStoredTransferCheckpoint(t, pool, copyID, 5, []byte("h"))
}

// TestTransferSourceVerificationUncleanHandoffResetsStoredCheckpoint はきれいでない引き継ぎが保存値を初期化することを確かめる。
func TestTransferSourceVerificationUncleanHandoffResetsStoredCheckpoint(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := testMedia(pool, "test-store", "")
	copyID, _ := progressTestCopy(t, pool)

	owner := acquireProgressTestLease(t, media, copyID)
	if err := media.BeginMediaAssetTransferSourceVerification(t.Context(), copyID, owner, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	saveAndReacquireProgressTestLease(t, media, copyID, owner, 9, []byte("h9"), nil)
	expireProgressLease(t, pool, copyID)
	acquireProgressTestLease(t, media, copyID)
	requireStoredTransferCheckpoint(t, pool, copyID, 0, nil)
}
