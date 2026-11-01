package pgstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
VALUES ($1, 'archive', $2, 0, 'pending')
RETURNING id`, assetID, "progress/"+uuid.NewString()+".ts").Scan(&copyID); err != nil {
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
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, first, 1, 5, state, nil); err != nil {
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
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, zombie, 1, 7, []byte("zombie"), nil); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("zombie Save = %v, want ErrTransferLeaseLost", err)
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
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, owner, 1, 3, []byte("late"), nil); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Save after own lease expiry = %v, want ErrTransferLeaseLost", err)
	}
}

// コピーを pending でなくす transaction と並行に進捗を取ると、その commit を待ってから state を読み直し、行を作らない。
// 待たずに古いスナップショットで作ると、pending でないコピーに行が残り、その世代の temp が GC で消えなくなる。
func TestTransferProgressAcquireWaitsForConcurrentCopyRetirement(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()

	retire, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retire.Rollback(ctx) }()
	if _, err := retire.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE id = $1`, copyID); err != nil {
		t.Fatalf("retiring copy: %v", err)
	}

	type result struct {
		acquired bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		_, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute)
		done <- result{acquired, err}
	}()
	// Acquire が retire の行ロックを待つまで待つ。待たずに終わったら（変異）、その結果で判定する。
	deadline := time.Now().Add(5 * time.Second)
	for blocked := false; !blocked; {
		if time.Now().After(deadline) {
			t.Fatal("Acquire neither blocked on the retiring copy nor finished")
		}
		if len(done) > 0 {
			break
		}
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%media_asset_copy_transfer_progress%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := retire.Commit(ctx); err != nil {
		t.Fatalf("committing retirement: %v", err)
	}
	got := <-done
	if got.err != nil || got.acquired {
		t.Fatalf("Acquire on a copy retired concurrently = (acquired=%t, %v), want not acquired", got.acquired, got.err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("progress rows for a retired copy = %d, want 0", rows)
	}
}

// uploadRow は進捗の行の送信状態と slice の状態を読む。行が無ければ ok=false。
func uploadRow(t *testing.T, pool *pgxpool.Pool, copyID int64) (uploadStore *string, uploadState []byte, offset int64, hashState []byte, handedOff, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
SELECT upload_store, upload_state, offset_bytes, hash_state, handed_off
FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&uploadStore, &uploadState, &offset, &hashState, &handedOff)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, 0, nil, false, false
	}
	if err != nil {
		t.Fatalf("reading progress row for copy %d: %v", copyID, err)
	}
	return uploadStore, uploadState, offset, hashState, handedOff, true
}

// abandonedStates は置き場 store の掃除待ちの送信状態を id 順に返す。
func abandonedStates(t *testing.T, media *Media, storeName string, limit int32) []string {
	t.Helper()
	rows, err := media.ListAbandonedTransferUploads(context.Background(), storeName, limit)
	if err != nil {
		t.Fatalf("ListAbandonedTransferUploads(%q): %v", storeName, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, string(r.State))
	}
	return out
}

// acquireLive は owner として進捗の lease を取り、取れたことを確かめる。
func acquireLive(t *testing.T, media *Media, copyID int64, owner uuid.UUID) store.TransferProgress {
	t.Helper()
	got, acquired, err := media.AcquireMediaAssetTransferProgress(context.Background(), copyID, owner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	return got
}

// TestTransferUploadReplaceAbandonsPreviousState は、送信状態の置き換えが古い状態を掃除待ちに移し、
// offset・SHA-256 の状態・handed_off を初期化することを確かめる。最初の置き換えは掃除待ちを作らない。
func TestTransferUploadReplaceAbandonsPreviousState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("first")); err != nil {
		t.Fatalf("first Replace: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 0 {
		t.Fatalf("abandoned after first Replace = %q, want none", got)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, owner, 1, 7, []byte("hash"), []byte("first")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 保存で lease を切ったので、次の試行が引き継いでから置き換える。
	next := uuid.New()
	acquireLive(t, media, copyID, next)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, next, 1, "archive", []byte("second")); err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 1 || got[0] != "first" {
		t.Fatalf("abandoned after second Replace = %q, want [first]", got)
	}
	storeName, state, offset, hash, handedOff, ok := uploadRow(t, pool, copyID)
	if !ok || storeName == nil || *storeName != "archive" || string(state) != "second" {
		t.Fatalf("row after second Replace = (store=%v, state=%q, ok=%t), want archive/second", storeName, state, ok)
	}
	if offset != 0 || hash != nil || handedOff {
		t.Fatalf("row after second Replace: offset=%d hash=%v handed_off=%t, want 0, nil, false", offset, hash, handedOff)
	}
}

// TestTransferUploadReplaceRequiresLiveLease は、lease を持たない試行の置き換えが ErrTransferLeaseLost を返し、
// 行も掃除待ちも変えないことを確かめる。
func TestTransferUploadReplaceRequiresLiveLease(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("first")); err != nil {
		t.Fatalf("first Replace: %v", err)
	}
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, uuid.New(), 1, "archive", []byte("stranger")); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Replace by a non-owner = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 2, "archive", []byte("wrong-generation")); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Replace with a wrong generation = %v, want ErrTransferLeaseLost", err)
	}
	expireProgressLease(t, pool, copyID)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("expired")); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Replace after own lease expiry = %v, want ErrTransferLeaseLost", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 0 {
		t.Fatalf("abandoned after rejected Replaces = %q, want none", got)
	}
	if _, state, _, _, _, _ := uploadRow(t, pool, copyID); string(state) != "first" {
		t.Fatalf("row state after rejected Replaces = %q, want first", state)
	}
}

// TestTransferUploadSavedStateReturnsOnlyOnCleanHandoff は、保存した送信状態がきれいな引き継ぎでだけ返り、
// きれいでない引き継ぎでは返らず行に残ることを確かめる。
func TestTransferUploadSavedStateReturnsOnlyOnCleanHandoff(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	ctx := context.Background()

	clean := progressTestCopy(t, pool)
	first := uuid.New()
	acquireLive(t, media, clean, first)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, clean, first, 1, "archive", []byte("up-1")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// 保存する送信状態は置き換えた時点と違ってよい（slice の間に増える）。Save が渡された値を書くことを確かめる。
	if err := media.SaveMediaAssetTransferProgress(ctx, clean, first, 1, 5, []byte("h"), []byte("up-1-saved")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := acquireLive(t, media, clean, uuid.New())
	if !got.Resumed || got.UploadStore != "archive" || string(got.UploadState) != "up-1-saved" {
		t.Fatalf("clean handoff = %+v, want resumed with archive/up-1-saved", got)
	}

	dirty := progressTestCopy(t, pool)
	stale := uuid.New()
	acquireLive(t, media, dirty, stale)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, dirty, stale, 1, "archive", []byte("up-2")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	expireProgressLease(t, pool, dirty)
	got = acquireLive(t, media, dirty, uuid.New())
	if got.Resumed || got.UploadStore != "" || got.UploadState != nil {
		t.Fatalf("dirty handoff = %+v, want a fresh generation without the upload state", got)
	}
	if storeName, state, _, _, _, ok := uploadRow(t, pool, dirty); !ok || storeName == nil || *storeName != "archive" || string(state) != "up-2" {
		t.Fatalf("row after dirty handoff = (store=%v, state=%q, ok=%t), want the stale archive/up-2 kept", storeName, state, ok)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 0 {
		t.Fatalf("abandoned after dirty handoff = %q, want none until Replace", got)
	}
}

// TestTransferUploadRowDeletionAbandonsState は、進捗の行が消えるとき（pending コピーの破棄、
// コピーの CASCADE 削除）に送信状態が掃除待ちに移り、送信状態の無い行では増えないことを確かめる。
func TestTransferUploadRowDeletionAbandonsState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	ctx := context.Background()

	discarded := progressTestCopy(t, pool)
	owner := uuid.New()
	acquireLive(t, media, discarded, owner)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, discarded, owner, 1, "archive", []byte("discard-me")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := media.DiscardPendingMediaAssetCopy(ctx, discarded); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 1 || got[0] != "discard-me" {
		t.Fatalf("abandoned after Discard = %q, want [discard-me]", got)
	}

	cascaded := progressTestCopy(t, pool)
	owner = uuid.New()
	acquireLive(t, media, cascaded, owner)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, cascaded, owner, 1, "archive", []byte("cascade-me")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE id = $1`, cascaded); err != nil {
		t.Fatalf("deleting copy: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 || got[1] != "cascade-me" {
		t.Fatalf("abandoned after copy CASCADE = %q, want [discard-me cascade-me]", got)
	}

	noUpload := progressTestCopy(t, pool)
	acquireLive(t, media, noUpload, uuid.New())
	if err := media.DiscardPendingMediaAssetCopy(ctx, noUpload); err != nil {
		t.Fatalf("Discard without upload: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 {
		t.Fatalf("abandoned after discarding a row without an upload = %q, want no new row", got)
	}
}

// TestTransferUploadAbandonedListFiltersByStoreInIDOrder は、掃除待ちの一覧が置き場で絞られ、id 順で limit 件まで返り、
// 削除で消えることを確かめる。
func TestTransferUploadAbandonedListFiltersByStoreInIDOrder(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	ctx := context.Background()
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	// 置き換えごとに、直前の状態が掃除待ちに移る。
	for _, step := range []struct{ store, state string }{
		{"archive", "x1"}, {"archive", "x2"}, {"other", "x3"}, {"archive", "x4"},
	} {
		if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, step.store, []byte(step.state)); err != nil {
			t.Fatalf("Replace %s/%s: %v", step.store, step.state, err)
		}
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 || got[0] != "x1" || got[1] != "x2" {
		t.Fatalf("archive abandoned = %q, want [x1 x2]", got)
	}
	if got := abandonedStates(t, media, "archive", 1); len(got) != 1 || got[0] != "x1" {
		t.Fatalf("archive abandoned with limit 1 = %q, want [x1]", got)
	}
	if got := abandonedStates(t, media, "other", 10); len(got) != 1 || got[0] != "x3" {
		t.Fatalf("other abandoned = %q, want [x3]", got)
	}
	rows, err := media.ListAbandonedTransferUploads(ctx, "archive", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := media.DeleteAbandonedTransferUpload(ctx, rows[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 1 || got[0] != "x2" {
		t.Fatalf("archive abandoned after delete = %q, want [x2]", got)
	}
}

// TestTransferUploadPairCheckRejectsHalfState は、送信置き場と送信状態の片方だけを持つ行を CHECK が拒むことを確かめる。
func TestTransferUploadPairCheckRejectsHalfState(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	copyID := progressTestCopy(t, pool)
	acquireLive(t, media, copyID, uuid.New())
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copy_transfer_progress SET upload_store = 'archive' WHERE copy_id = $1`, copyID); err == nil {
		t.Fatal("setting only upload_store succeeded, want a CHECK violation")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copy_transfer_progress SET upload_state = '\x01'::bytea WHERE copy_id = $1`, copyID); err == nil {
		t.Fatal("setting only upload_state succeeded, want a CHECK violation")
	}
}
