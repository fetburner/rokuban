package pgstore

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// progressTestCopy は pending の転送先コピーを 1 件作り、その copy ID と asset ID を返す。
func progressTestCopy(t *testing.T, pool *pgxpool.Pool) (copyID, assetID int64) {
	t.Helper()
	recordingID := dbtest.Recording(t, pool)
	assetID = dbtest.MediaAsset(t, pool, recordingID)
	if err := pool.QueryRow(context.Background(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, 'archive', $2, 0, 'pending')
RETURNING id`, assetID, "progress/"+uuid.NewString()+".ts").Scan(&copyID); err != nil {
		t.Fatalf("inserting pending copy: %v", err)
	}
	return copyID, assetID
}

// expireProgressLease は DB の時計を基準に lease の期限を過去にする。
func expireProgressLease(t *testing.T, pool *pgxpool.Pool, copyID int64) {
	t.Helper()
	execTransferFixture(t, pool, `UPDATE media_asset_copy_transfer_progress
SET lease_expires_at = now() - interval '1 second' WHERE copy_id = $1`, copyID)
}

func acquireProgressTestLease(t *testing.T, media *Media, copyID int64) uuid.UUID {
	t.Helper()
	owner := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(t.Context(), copyID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	return owner
}

// saveAndReacquireProgressTestLease は世代 1 の保存値を引き継いだ持ち主を作る。
func saveAndReacquireProgressTestLease(t *testing.T, media *Media, copyID int64, owner uuid.UUID, offset int64, hash, upload []byte) uuid.UUID {
	t.Helper()
	if err := media.SaveMediaAssetTransferProgress(t.Context(), copyID, owner, 1, offset, hash, upload); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return acquireProgressTestLease(t, media, copyID)
}

func requireStoredTransferCheckpoint(t *testing.T, pool *pgxpool.Pool, copyID, wantOffset int64, wantHash []byte) {
	t.Helper()
	var offset int64
	var hash []byte
	if err := pool.QueryRow(t.Context(), `SELECT offset_bytes, hash_state
FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&offset, &hash); err != nil {
		t.Fatalf("reading stored checkpoint for copy %d: %v", copyID, err)
	}
	if offset != wantOffset || (hash == nil) != (wantHash == nil) || !bytes.Equal(hash, wantHash) {
		t.Fatalf("stored checkpoint for copy %d = (%d, %#v), want (%d, %#v)", copyID, offset, hash, wantOffset, wantHash)
	}
}

// TestTransferUploadReplaceResetsStoredCheckpoint は送信状態を置き換えた直後の保存値を確かめる。
func TestTransferUploadReplaceResetsStoredCheckpoint(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := testMedia(pool, "test-store", "")
	copyID, _ := progressTestCopy(t, pool)
	ctx := t.Context()
	owner := acquireProgressTestLease(t, media, copyID)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("first")); err != nil {
		t.Fatalf("first Replace: %v", err)
	}
	owner = saveAndReacquireProgressTestLease(t, media, copyID, owner, 7, []byte("hash"), []byte("first"))
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("second")); err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	requireStoredTransferCheckpoint(t, pool, copyID, 0, nil)
}

// TestTransferUploadPairCheckRejectsHalfState は、送信置き場と送信状態の片方だけを持つ行を CHECK が拒むことを確かめる。
func TestTransferUploadPairCheckRejectsHalfState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := testMedia(pool, "test-store", "")
	copyID, _ := progressTestCopy(t, pool)
	acquireProgressTestLease(t, media, copyID)
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copy_transfer_progress SET upload_store = 'archive' WHERE copy_id = $1`, copyID); err == nil {
		t.Fatal("setting only upload_store succeeded, want a CHECK violation")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copy_transfer_progress SET upload_state = '\x01'::bytea WHERE copy_id = $1`, copyID); err == nil {
		t.Fatal("setting only upload_state succeeded, want a CHECK violation")
	}
}

// TestTransferSourceVerificationExcludesUploadState は、照合中の行に派生物の送信状態が同居しないことを、CHECK が止めることを確かめる。
func TestTransferSourceVerificationExcludesUploadState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := testMedia(pool, "test-store", "")
	copyID, _ := progressTestCopy(t, pool)
	owner := acquireProgressTestLease(t, media, copyID)
	if err := media.ReplaceMediaAssetTransferUpload(context.Background(), copyID, owner, 1, "archive", []byte("up")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copy_transfer_progress SET verifying_source = true WHERE copy_id = $1`, copyID); err == nil {
		t.Fatal("verifying_source = true alongside an upload state was accepted, want the CHECK to reject it")
	}
}
