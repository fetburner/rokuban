package pgstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// verifyingFlag は copyID の進捗の行の verifying_source を返す。
func verifyingFlag(t *testing.T, media *Media, copyID int64) bool {
	t.Helper()
	var v bool
	if err := media.pool.QueryRow(context.Background(), `SELECT verifying_source FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&v); err != nil {
		t.Fatalf("reading verifying_source for copy %d: %v", copyID, err)
	}
	return v
}

// TestRecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration は、観測した SHA-256 が、SHA-256 が未知で
// 世代が一致する論理資産にだけ書かれ、値のある行や世代の違う行には書かれないことを確かめる。
func TestRecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	var assetID int64
	if err := pool.QueryRow(ctx, `SELECT media_asset_id FROM media_asset_copies WHERE id = $1`, copyID).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	readSHA := func() []byte {
		t.Helper()
		var sum []byte
		if err := pool.QueryRow(ctx, `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	observed := []byte("0123456789abcdef0123456789abcdef")
	other := []byte("fedcba9876543210fedcba9876543210")

	if recorded, err := media.RecordObservedMediaAssetSHA256(ctx, assetID, 1, observed); err != nil || recorded {
		t.Fatalf("Record for a different generation = (%t, %v), want not recorded", recorded, err)
	}
	if got := readSHA(); got != nil {
		t.Fatalf("SHA-256 after a different-generation Record = %x, want NULL", got)
	}
	if recorded, err := media.RecordObservedMediaAssetSHA256(ctx, assetID, 0, observed); err != nil || !recorded {
		t.Fatalf("Record for the current generation = (%t, %v), want recorded", recorded, err)
	}
	if got := readSHA(); !bytes.Equal(got, observed) {
		t.Fatalf("SHA-256 after Record = %x, want %x", got, observed)
	}
	if recorded, err := media.RecordObservedMediaAssetSHA256(ctx, assetID, 0, other); err != nil || recorded {
		t.Fatalf("Record over a known SHA-256 = (%t, %v), want not recorded", recorded, err)
	}
	if got := readSHA(); !bytes.Equal(got, observed) {
		t.Fatalf("known SHA-256 after a second Record = %x, want it kept as %x", got, observed)
	}
}

// TestTransferSourceVerificationRequiresLiveLease は、照合の開始と終わりが lease を持つ試行だけに許され、
// 拒まれた呼び出しが行を変えないことを確かめる。
func TestTransferSourceVerificationRequiresLiveLease(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	stranger := uuid.New()

	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, stranger, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin by a non-owner = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 2); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin with a wrong generation = %v, want ErrTransferLeaseLost", err)
	}
	if verifyingFlag(t, media, copyID) {
		t.Fatal("rejected Begin set verifying_source")
	}
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); err != nil {
		t.Fatalf("Begin by the owner: %v", err)
	}
	if !verifyingFlag(t, media, copyID) {
		t.Fatal("Begin by the owner did not set verifying_source")
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, stranger, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("End by a non-owner = %v, want ErrTransferLeaseLost", err)
	}
	// 期限の切れた持ち主の Begin / End は拒まれ、行を変えない。
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copy_transfer_progress SET offset_bytes = 5, hash_state = 'h' WHERE copy_id = $1`, copyID); err != nil {
		t.Fatal(err)
	}
	expireProgressLease(t, pool, copyID)
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin after the owner's lease expired = %v, want ErrTransferLeaseLost", err)
	}
	var offset int64
	if err := pool.QueryRow(ctx, `SELECT offset_bytes FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&offset); err != nil || offset != 5 {
		t.Fatalf("rejected Begin changed offset to %d (%v), want 5", offset, err)
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("End after the owner's lease expired = %v, want ErrTransferLeaseLost", err)
	}
	if !verifyingFlag(t, media, copyID) {
		t.Fatal("rejected End cleared verifying_source")
	}

	// 期限の切れた lease は次の試行が引き継ぐ（きれいでない引き継ぎ）。照合は続くので、終わりは引き継いだ試行が書く。
	nextOwner := uuid.New()
	next := acquireLive(t, media, copyID, nextOwner)
	if !verifyingFlag(t, media, copyID) || !next.VerifyingSource {
		t.Fatalf("verification after an unclean handoff = (row %t, progress %t), want still verifying", verifyingFlag(t, media, copyID), next.VerifyingSource)
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, nextOwner, next.Generation); err != nil {
		t.Fatalf("End by the new owner: %v", err)
	}
	if verifyingFlag(t, media, copyID) {
		t.Fatal("End by the owner did not clear verifying_source")
	}
}

// TestTransferSourceVerificationSurvivesHandoffs は、照合中の進捗が、きれいな引き継ぎでは保存した位置を、
// きれいでない引き継ぎでは位置と状態を 0 / nil に戻して、どちらも照合中のまま返ることを確かめる。
func TestTransferSourceVerificationSurvivesHandoffs(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	first := uuid.New()
	acquireLive(t, media, copyID, first)
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, first, 1); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, first, 1, 7, []byte("h7"), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second := uuid.New()
	got := acquireLive(t, media, copyID, second)
	if !got.Resumed || !got.VerifyingSource || got.Offset != 7 || string(got.HashState) != "h7" || got.Generation != 1 {
		t.Fatalf("clean handoff = %+v, want resumed verification at offset 7 in generation 1", got)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, second, 1, 9, []byte("h9"), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	third := uuid.New()
	got = acquireLive(t, media, copyID, third)
	if !got.Resumed || !got.VerifyingSource || got.Offset != 9 || string(got.HashState) != "h9" {
		t.Fatalf("second clean handoff = %+v, want resumed verification at offset 9", got)
	}

	// 3 つめの持ち主は保存せずに止まる（lease だけが切れる）。次の試行はきれいでない引き継ぎになる。
	expireProgressLease(t, pool, copyID)
	got = acquireLive(t, media, copyID, uuid.New())
	if got.Resumed || !got.VerifyingSource || got.Offset != 0 || got.HashState != nil || got.Generation != 2 {
		t.Fatalf("unclean handoff = %+v, want an unresumed verification from 0 in generation 2", got)
	}
	var offset int64
	var hash []byte
	if err := pool.QueryRow(ctx, `SELECT offset_bytes, hash_state FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&offset, &hash); err != nil {
		t.Fatal(err)
	}
	if offset != 0 || hash != nil || !verifyingFlag(t, media, copyID) {
		t.Fatalf("row after unclean handoff = (offset %d, hash %v, verifying %t), want (0, nil, true)", offset, hash, verifyingFlag(t, media, copyID))
	}
}

// TestTransferSourceVerificationExcludesUploadState は、照合中の行に派生物の送信状態が同居しないことを、CHECK が止めることを確かめる。
func TestTransferSourceVerificationExcludesUploadState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	if err := media.ReplaceMediaAssetTransferUpload(context.Background(), copyID, owner, 1, "archive", []byte("up")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copy_transfer_progress SET verifying_source = true WHERE copy_id = $1`, copyID); err == nil {
		t.Fatal("verifying_source = true alongside an upload state was accepted, want the CHECK to reject it")
	}
}
