package contracttest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

func runTransferProgressFreshRowStartsGenerationOne(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	got, acquired, err := media.AcquireMediaAssetTransferProgress(t.Context(), copyID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if got.WriterGeneration != 1 || got.PrevWriterGeneration != 0 || got.Resumed || got.Offset != 0 || got.HashState != nil {
		t.Fatalf("Acquire progress = %+v, want generation 1 with no previous generation", got)
	}
	if generation, ok, err := media.GetMediaAssetTransferProgressWriterGeneration(t.Context(), copyID); err != nil || !ok || generation != 1 {
		t.Fatalf("stored generation = (%d, %t, %v), want 1", generation, ok, err)
	}
}

func runTransferProgressLiveLeaseBlocksOtherOwner(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("second Acquire = (acquired=%t, %v), want busy", acquired, err)
	}
}

func runTransferProgressCleanHandoffResumesSavedState(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
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
	if !got.Resumed || got.WriterGeneration != 1 || got.PrevWriterGeneration != 1 || got.Offset != 5 || !bytes.Equal(got.HashState, state) {
		t.Fatalf("Acquire after handoff = %+v, want resumed generation 1 at offset 5 with the saved state", got)
	}
}

func runTransferProgressExpiredLeaseStartsNewGeneration(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	f.ExpireProgressLease(t, copyID)
	got, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after expiry = (acquired=%t, %v), want acquired", acquired, err)
	}
	if got.Resumed || got.WriterGeneration != 2 || got.PrevWriterGeneration != 1 || got.Offset != 0 || got.HashState != nil {
		t.Fatalf("Acquire after expiry = %+v, want generation 2 from the start", got)
	}
}

func runTransferProgressZombieSaveIsRejected(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
	zombie := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, zombie, time.Minute); err != nil || !acquired {
		t.Fatalf("zombie Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	f.ExpireProgressLease(t, copyID)
	successor := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, successor, time.Minute); err != nil || !acquired {
		t.Fatalf("successor Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, zombie, 1, 7, []byte("zombie"), nil); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("zombie Save = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, zombie, 2, 7, []byte("zombie"), nil); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Save by the zombie with the current generation = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, successor, 1, 7, []byte("zombie"), nil); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Save by the successor with the old generation = %v, want ErrTransferLeaseLost", err)
	}
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after rejected Saves = (%t, %v), want busy", acquired, err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, successor, 2, 3, []byte("current"), nil); err != nil {
		t.Fatalf("Save by the successor after rejected Saves: %v", err)
	}
}

func runTransferProgressRowIsDeletedWithPendingCopy(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if _, ok, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, copyID); err != nil || !ok {
		t.Fatalf("progress before discard = (ok=%t, %v), want a row", ok, err)
	}
	if err := media.DiscardPendingMediaAssetCopy(ctx, copyID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, ok, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, copyID); err != nil || ok {
		t.Fatalf("progress after discard = (ok=%t, %v), want no row", ok, err)
	}
}

// runTransferProgressSaveAfterOwnLeaseExpiryIsRejected は、持ち主が期限切れの後に、誰も引き継がないまま保存することを拒むことを確かめる。
// 引き継ぎの後のゾンビは owner の照合で落ちるので、この期限の照合はそれとは別に効く。
func runTransferProgressSaveAfterOwnLeaseExpiryIsRejected(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
	owner := uuid.New()
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	f.ExpireProgressLease(t, copyID)
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, owner, 1, 3, []byte("late"), nil); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Save after own lease expiry = %v, want ErrTransferLeaseLost", err)
	}
	if got := acquireLive(t, media, copyID, uuid.New()); got.Resumed || got.WriterGeneration != 2 || got.PrevWriterGeneration != 1 || got.Offset != 0 || got.HashState != nil {
		t.Fatalf("Acquire after rejected Save = %+v, want unclean handoff to generation 2", got)
	}
}

// runTransferUploadReplaceAbandonsPreviousState は、送信状態の置き換えが古い状態を掃除待ちに移し、
// 次の試行に lease を渡さないことを確かめる。最初の置き換えは掃除待ちを作らない。
func runTransferUploadReplaceAbandonsPreviousState(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
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
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after Replace = (acquired=%t, %v), want a live lease without handoff", acquired, err)
	}
	if err := media.DiscardPendingMediaAssetCopy(ctx, copyID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 || got[1] != "second" {
		t.Fatalf("abandoned after Discard = %q, want [first second]", got)
	}
}

// runTransferUploadReplaceRequiresLiveLease は、lease を持たない試行の置き換えが ErrTransferLeaseLost を返し、
// lease と送信状態を変えないことを確かめる。
func runTransferUploadReplaceRequiresLiveLease(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
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
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after rejected Replaces = (%t, %v), want busy", acquired, err)
	}
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("second")); err != nil {
		t.Fatalf("Replace by the owner after rejected Replaces: %v", err)
	}
	f.ExpireProgressLease(t, copyID)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, copyID, owner, 1, "archive", []byte("expired")); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Replace after own lease expiry = %v, want ErrTransferLeaseLost", err)
	}
	if got := acquireLive(t, media, copyID, uuid.New()); got.Resumed || got.WriterGeneration != 2 || got.PrevWriterGeneration != 1 {
		t.Fatalf("Acquire after rejected expired Replace = %+v, want unclean handoff to generation 2", got)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 1 || got[0] != "first" {
		t.Fatalf("abandoned after rejected Replaces and the owner's Replace = %q, want [first]", got)
	}
	if err := media.DiscardPendingMediaAssetCopy(ctx, copyID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("abandoned after Discard = %q, want [first second]", got)
	}
}

// runTransferUploadSavedStateReturnsOnlyOnCleanHandoff は、保存した送信状態がきれいな引き継ぎでだけ返り、
// きれいでない引き継ぎでは返らず行に残ることを確かめる。
func runTransferUploadSavedStateReturnsOnlyOnCleanHandoff(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	ctx := t.Context()

	clean, _ := f.NewPendingCopy(t)
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

	dirty, _ := f.NewPendingCopy(t)
	stale := uuid.New()
	acquireLive(t, media, dirty, stale)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, dirty, stale, 1, "archive", []byte("up-2")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	f.ExpireProgressLease(t, dirty)
	next := uuid.New()
	got = acquireLive(t, media, dirty, next)
	if got.Resumed || got.UploadStore != "" || got.UploadState != nil {
		t.Fatalf("dirty handoff = %+v, want a fresh generation without the upload state", got)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 0 {
		t.Fatalf("abandoned after dirty handoff = %q, want none until Replace", got)
	}
	if err := media.ReplaceMediaAssetTransferUpload(ctx, dirty, next, 2, "archive", []byte("up-3")); err != nil {
		t.Fatalf("Replace after dirty handoff: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 1 || got[0] != "up-2" {
		t.Fatalf("abandoned after replacing the stale upload = %q, want [up-2]", got)
	}
}

// runTransferUploadRowDeletionAbandonsState は、進捗の行が消えるとき（pending コピーの破棄、
// コピーの CASCADE 削除）に送信状態が掃除待ちに移り、送信状態の無い行では増えないことを確かめる。
func runTransferUploadRowDeletionAbandonsState(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	ctx := t.Context()

	discarded, _ := f.NewPendingCopy(t)
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

	cascaded, _ := f.NewPendingCopy(t)
	owner = uuid.New()
	acquireLive(t, media, cascaded, owner)
	if err := media.ReplaceMediaAssetTransferUpload(ctx, cascaded, owner, 1, "archive", []byte("cascade-me")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	f.DeleteCopy(t, cascaded)
	if _, ok, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, cascaded); err != nil || ok {
		t.Fatalf("progress after copy CASCADE = (%t, %v), want no row", ok, err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 || got[0] != "discard-me" || got[1] != "cascade-me" {
		t.Fatalf("abandoned after copy CASCADE = %q, want [discard-me cascade-me]", got)
	}

	noUpload, _ := f.NewPendingCopy(t)
	acquireLive(t, media, noUpload, uuid.New())
	if err := media.DiscardPendingMediaAssetCopy(ctx, noUpload); err != nil {
		t.Fatalf("Discard without upload: %v", err)
	}
	if got := abandonedStates(t, media, "archive", 10); len(got) != 2 {
		t.Fatalf("abandoned after discarding a row without an upload = %q, want no new row", got)
	}
}

// runTransferUploadAbandonedListFiltersByStoreInIDOrder は、掃除待ちの一覧が置き場で絞られ、id 順で limit 件まで返り、
// 削除で消えることを確かめる。
func runTransferUploadAbandonedListFiltersByStoreInIDOrder(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
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

// runTransferSourceVerificationRequiresLiveLease は、照合の開始と終わりが lease を持つ試行だけに許され、
// 拒まれた呼び出しが lease と照合フラグを変えないことを確かめる。
func runTransferSourceVerificationRequiresLiveLease(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
	owner := uuid.New()
	acquireLive(t, media, copyID, owner)
	stranger := uuid.New()

	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, stranger, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin by a non-owner = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 2); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin with a wrong generation = %v, want ErrTransferLeaseLost", err)
	}
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after rejected Begins = (%t, %v), want busy", acquired, err)
	}
	// Begin が拒まれた時点の照合フラグを、同じ世代のきれいな引き継ぎで読む。
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, owner, 1, 0, nil, nil); err != nil {
		t.Fatalf("Save after rejected Begin: %v", err)
	}
	if got := acquireLive(t, media, copyID, owner); got.VerifyingSource {
		t.Fatal("rejected Begin set verifying_source")
	}
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); err != nil {
		t.Fatalf("Begin by the owner: %v", err)
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, stranger, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("End by a non-owner = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, owner, 2); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("End with a wrong generation = %v, want ErrTransferLeaseLost", err)
	}
	if _, acquired, err := media.AcquireMediaAssetTransferProgress(ctx, copyID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire after rejected Ends = (%t, %v), want busy", acquired, err)
	}
	if err := media.SaveMediaAssetTransferProgress(ctx, copyID, owner, 1, 5, []byte("h"), nil); err != nil {
		t.Fatalf("Save after rejected Ends: %v", err)
	}
	if got := acquireLive(t, media, copyID, owner); !got.Resumed || got.WriterGeneration != 1 || !got.VerifyingSource {
		t.Fatalf("Acquire after rejected Ends and Save = %+v, want resumed verification in generation 1", got)
	}

	f.ExpireProgressLease(t, copyID)
	if err := media.BeginMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("Begin after the owner's lease expired = %v, want ErrTransferLeaseLost", err)
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, owner, 1); !errors.Is(err, store.ErrTransferLeaseLost) {
		t.Fatalf("End after the owner's lease expired = %v, want ErrTransferLeaseLost", err)
	}

	// 期限の切れた lease は次の試行が引き継ぐ（きれいでない引き継ぎ）。照合は続くので、終わりは引き継いだ試行が書く。
	nextOwner := uuid.New()
	next := acquireLive(t, media, copyID, nextOwner)
	if next.Resumed || next.WriterGeneration != 2 || next.PrevWriterGeneration != 1 || !next.VerifyingSource {
		t.Fatalf("Acquire after rejected expired Begin and End = %+v, want unclean handoff to generation 2 with verification", next)
	}
	if err := media.EndMediaAssetTransferSourceVerification(ctx, copyID, nextOwner, 2); err != nil {
		t.Fatalf("End by the new owner: %v", err)
	}
	if err := media.ReleaseMediaAssetTransferProgress(ctx, copyID, nextOwner); err != nil {
		t.Fatalf("Release after End: %v", err)
	}
	if got := acquireLive(t, media, copyID, uuid.New()); got.VerifyingSource {
		t.Fatal("End by the owner did not clear verifying_source")
	}
}

// runTransferSourceVerificationSurvivesHandoffs は、照合中の進捗が、きれいな引き継ぎでは保存した位置を、
// きれいでない引き継ぎでは位置と状態を 0 / nil に戻して、どちらも照合中のまま返ることを確かめる。
func runTransferSourceVerificationSurvivesHandoffs(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	copyID, _ := f.NewPendingCopy(t)
	ctx := t.Context()
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
	if !got.Resumed || !got.VerifyingSource || got.Offset != 7 || string(got.HashState) != "h7" || got.WriterGeneration != 1 {
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
	f.ExpireProgressLease(t, copyID)
	got = acquireLive(t, media, copyID, uuid.New())
	if got.Resumed || !got.VerifyingSource || got.Offset != 0 || got.HashState != nil || got.WriterGeneration != 2 {
		t.Fatalf("unclean handoff = %+v, want an unresumed verification from 0 in generation 2", got)
	}
	f.ExpireProgressLease(t, copyID)
	if got := acquireLive(t, media, copyID, uuid.New()); !got.VerifyingSource {
		t.Fatal("a second unclean handoff lost the persisted verification flag")
	}
}

// runRecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration は、観測した SHA-256 が、SHA-256 が未知で
// 世代が一致する論理資産にだけ書かれ、値のある行や世代の違う行には書かれないことを確かめる。
func runRecordObservedMediaAssetSHA256WritesOnlyUnknownCurrentGeneration(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	copyID, assetID := f.NewPendingCopy(t)
	ctx := t.Context()
	stores := []string{"test-store", "archive"}
	media := f.WithPolicy("test-store", []store.OriginalStoreRule{{Site: "default", Store: "archive"}}, stores, "")
	readSHA := func() []byte {
		t.Helper()
		transfer, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "archive", stores, stores, stores, 10)
		if err != nil || !ok || transfer.CopyID != copyID {
			t.Fatalf("Prepare to read SHA-256 = (%+v, %t, %v), want existing pending copy %d", transfer, ok, err, copyID)
		}
		return transfer.SHA256
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

func abandonedStates(t *testing.T, media store.StorageTransfer, storeName string, limit int32) []string {
	t.Helper()
	rows, err := media.ListAbandonedTransferUploads(t.Context(), storeName, limit)
	if err != nil {
		t.Fatalf("ListAbandonedTransferUploads(%q): %v", storeName, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, string(r.State))
	}
	return out
}

func acquireLive(t *testing.T, media store.StorageTransfer, copyID int64, owner uuid.UUID) store.TransferProgress {
	t.Helper()
	got, acquired, err := media.AcquireMediaAssetTransferProgress(t.Context(), copyID, owner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	return got
}
