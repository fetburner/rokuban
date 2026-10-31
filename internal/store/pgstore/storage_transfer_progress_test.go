package pgstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// progressTestCopy は pending の転送先コピーを 1 件作り、その copy ID を返す。
func progressTestCopy(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	var copyID int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, 'archive', 'progress/test.ts', 0, 'pending')
RETURNING id`, assetID).Scan(&copyID); err != nil {
		t.Fatalf("inserting pending copy: %v", err)
	}
	return copyID
}

// expireProgressLease は lease の期限を過去にし、持ち主が止まったときと同じ状態にする。
func expireProgressLease(t *testing.T, pool *pgxpool.Pool, copyID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE media_asset_copy_transfer_progress SET lease_expires_at = now() - interval '1 second' WHERE copy_id = $1`, copyID); err != nil {
		t.Fatalf("expiring progress lease: %v", err)
	}
}

func TestTransferProgressFreshRowStartsGenerationOne(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	got, acquired, err := media.AcquireMediaAssetTransferProgress(context.Background(), copyID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if got.Generation != 1 || got.PrevGeneration != 0 || got.Resumed || got.Offset != 0 || got.HashState != nil {
		t.Fatalf("Acquire progress = %+v, want generation 1 with no previous generation", got)
	}
}

func TestTransferProgressLiveLeaseBlocksOtherOwner(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("second Acquire = (acquired=%t, %v), want busy", acquired, err)
	}
}

func TestTransferProgressCleanHandoffResumesSavedState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	first := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, first, time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	state := []byte("saved-sha256-state")
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, first, 1, 5, state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after handoff = (acquired=%t, %v), want acquired", acquired, err)
	}
	if !got.Resumed || got.Generation != 1 || got.PrevGeneration != 1 || got.Offset != 5 || !bytes.Equal(got.HashState, state) {
		t.Fatalf("Acquire after handoff = %+v, want resumed generation 1 at offset 5 with the saved state", got)
	}
}

func TestTransferProgressExpiredLeaseStartsNewGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	expireProgressLease(t, pool, copyID)
	got, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after expiry = (acquired=%t, %v), want acquired", acquired, err)
	}
	if got.Resumed || got.Generation != 2 || got.PrevGeneration != 1 || got.Offset != 0 || got.HashState != nil {
		t.Fatalf("Acquire after expiry = %+v, want generation 2 from the start", got)
	}
}

func TestTransferProgressZombieSaveIsRejected(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	zombie := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, zombie, time.Minute); err != nil || !acquired {
		t.Fatalf("zombie Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	expireProgressLease(t, pool, copyID)
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("successor Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, zombie, 1, 7, []byte("zombie")); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("zombie Save = %v, want ErrTransferLeaseLost", err)
	}
	if _, err := media.ResetMediaAssetTransferProgress(ctx, copyID, zombie); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("zombie Reset = %v, want ErrTransferLeaseLost", err)
	}
}

func TestTransferProgressRowIsDeletedWithPendingCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if _, ok, err := media.GetMediaAssetTransferProgressGeneration(ctx, copyID); err != nil || !ok {
		t.Fatalf("progress before discard = (ok=%t, %v), want a row", ok, err)
	}
	if err := media.DiscardPendingMediaAssetCopy(ctx, copyID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, ok, err := media.GetMediaAssetTransferProgressGeneration(ctx, copyID); err != nil || ok {
		t.Fatalf("progress after discard = (ok=%t, %v), want no row", ok, err)
	}
}

// TestTransferProgressSaveAfterOwnLeaseExpiryIsRejected は、持ち主が期限切れの後に、誰も引き継がないまま保存することを拒むことを確かめる。
// 引き継ぎの後のゾンビは owner の照合で落ちるので、この期限の照合はそれとは別に効く。
func TestTransferProgressSaveAfterOwnLeaseExpiryIsRejected(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	owner := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	expireProgressLease(t, pool, copyID)
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, owner, 1, 3, []byte("late")); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Save after own lease expiry = %v, want ErrTransferLeaseLost", err)
	}
}
