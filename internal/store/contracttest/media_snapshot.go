package contracttest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// interleaveTimeout は読み手の tx の途中で行う書き込みの制限時間である。
// 読み取りが書き込みを待たせる（読み取りがロックを取っている）ときは、ここで失敗させる。
const interleaveTimeout = 10 * time.Second

// interleaveWrite は、読み手の読み取りの途中で write を別の goroutine で実行する関数と、その呼び出し回数を返す。
// write が制限時間内に終わらなければ t.Error で失敗させる。テストの終了時には、実行中の write が終わるまで待つ。
func interleaveWrite(t *testing.T, write func()) (func(), *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	interleave := func() {
		calls.Add(1)
		done := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			write()
		}()
		select {
		case <-done:
		case <-time.After(interleaveTimeout):
			t.Errorf("reader blocked a concurrent writer for %s; a snapshot read must not take locks", interleaveTimeout)
		}
	}
	return interleave, &calls
}

// snapshotChapterSpans は、ユーザー所有の区間で 2 本の keep を導く（runCurrentCutKeep と同じ区間）。
var snapshotChapterSpans = []chapters.Span{
	{StartMs: 0, EndMs: 30030, Label: "main"},
	{StartMs: 30030, EndMs: 45045, Label: "CM", Cut: true},
	{StartMs: 45045, EndMs: 60060, Label: "end"},
}

// runCurrentCutKeepSnapshot は、GetCurrentCutKeep が所有状態と区間を同じ時点で読むことを検査する。
// 読み手の最初の SELECT（所有状態）の後で、別の接続がチャプターの所有を外す。割り込み前の値と完全に一致しなければ失敗する。
//
// 分離レベルの違いは判定に持ち込まない。各 DB の文書では、PostgreSQL の REPEATABLE READ は tx 内の最初の問い合わせ（トランザクション制御文を除く）でスナップショットを取り、
// MySQL の REPEATABLE READ は最初の一貫性読み取りで取る。MySQL の接続の既定は READ-COMMITTED で、store が tx ごとに REPEATABLE READ を指定する。
// このテストは、両 DB で同じ判定により、最初の SELECT の後の書き込みが見えないことを確かめる。
func runCurrentCutKeepSnapshot(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	id := f.NewRecording(t)
	f.SetChapterSpans(t, id, snapshotChapterSpans)
	want := []chapters.Range{{StartMs: 0, EndMs: 30030}, {StartMs: 45045, EndMs: 60060}}
	before, owned, err := f.Reconcile.GetCurrentCutKeep(ctx, id)
	if err != nil || !owned || !chapters.SameRanges(before, want) {
		t.Fatalf("GetCurrentCutKeep(%d) before the interleaved reset = (%v, %v, %v), want (%v, true, nil)", id, before, owned, err, want)
	}

	interleave, calls := interleaveWrite(t, func() {
		if err := f.Chapters.Reset(context.Background(), id); err != nil {
			t.Errorf("resetting chapters for recording %d during the read: %v", id, err)
		}
	})
	got, gotOwned, err := f.NewInterleavedReconcile(t, interleave).GetCurrentCutKeep(ctx, id)
	if n := calls.Load(); n != 1 {
		t.Fatalf("interleaved reset ran %d times, want 1", n)
	}
	if err != nil || gotOwned != owned || !chapters.SameRanges(got, before) {
		t.Fatalf("GetCurrentCutKeep(%d) across the reset = (%v, %v, %v), want the pre-reset (%v, %v, nil)", id, got, gotOwned, err, before, owned)
	}

	// 割り込みが実際に値を動かしたことの確認。動かなければ、一致の判定は何も主張していない。
	after, afterOwned, err := f.Reconcile.GetCurrentCutKeep(ctx, id)
	if err != nil || afterOwned || len(after) != 0 {
		t.Fatalf("GetCurrentCutKeep(%d) after the reset = (%v, %v, %v), want (nil, false, nil)", id, after, afterOwned, err)
	}
}

// encodedPublishStateFields は EncodedPublishState の全フィールドの名前である。
var encodedPublishStateFields = []string{
	"Desired", "HasRebuildRequest", "RebuildRequestKeep", "ChapterOwned",
	"CurrentCutKeep", "EncodedAsset", "MaxCopyGeneration",
}

// encodedPublishStateDiff は a と b で値の違うフィールドの名前を返す。全フィールドを比べる。
func encodedPublishStateDiff(a, b store.EncodedPublishState) []string {
	var diff []string
	if a.Desired != b.Desired {
		diff = append(diff, "Desired")
	}
	if a.HasRebuildRequest != b.HasRebuildRequest {
		diff = append(diff, "HasRebuildRequest")
	}
	if !chapters.SameRanges(a.RebuildRequestKeep, b.RebuildRequestKeep) {
		diff = append(diff, "RebuildRequestKeep")
	}
	if a.ChapterOwned != b.ChapterOwned {
		diff = append(diff, "ChapterOwned")
	}
	if !chapters.SameRanges(a.CurrentCutKeep, b.CurrentCutKeep) {
		diff = append(diff, "CurrentCutKeep")
	}
	if !sameEncodedAssetObservation(a.EncodedAsset, b.EncodedAsset) {
		diff = append(diff, "EncodedAsset")
	}
	if !sameInt64Ptr(a.MaxCopyGeneration, b.MaxCopyGeneration) {
		diff = append(diff, "MaxCopyGeneration")
	}
	return diff
}

func sameEncodedAssetObservation(a, b *store.EncodedAssetObservation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.Active == b.Active && a.DerivedGeneration == b.DerivedGeneration &&
		chapters.SameRanges(a.KeepRanges, b.KeepRanges)
}

func sameInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// runEncodedPublishStateSnapshot は、GetEncodedPublishState が返す全項目が 1 つの時点を見ることを検査する。
// 読み手の最初の SELECT の後で、全項目を別の接続から同時に動かす。割り込み前の値と全フィールドで一致しなければ失敗する。
// 分離レベルの扱いは、runCurrentCutKeepSnapshot の doc コメントと同じで、判定には持ち込まない。
func runEncodedPublishStateSnapshot(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := t.Context()
	id := f.NewRecording(t)
	profile := "cut"
	f.SetEncodeProfiles(t, id, []string{profile})
	f.SetChapterSpans(t, id, snapshotChapterSpans)
	assetID := f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/cut.mp4", id), "active")
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	f.SetMediaAssetKeepRanges(t, assetID, keep)
	f.AddTombstoneCopy(t, assetID, "osaka", fmt.Sprintf("sites/home/%d/cut-osaka.mp4", id), 3)

	wantKeep := []chapters.Range{{StartMs: 0, EndMs: 30030}, {StartMs: 45045, EndMs: 60060}}
	before, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, true)
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q) before the interleaved writes: %v", id, profile, err)
	}
	if !before.Desired || before.HasRebuildRequest || !before.ChapterOwned || !chapters.SameRanges(before.CurrentCutKeep, wantKeep) ||
		before.EncodedAsset == nil || before.EncodedAsset.ID != assetID || !before.EncodedAsset.Active ||
		!chapters.SameRanges(before.EncodedAsset.KeepRanges, keep) ||
		before.MaxCopyGeneration == nil || *before.MaxCopyGeneration != 3 {
		t.Fatalf("GetEncodedPublishState(%d, %q) before the interleaved writes = %+v, want desired, owned chapters, active asset %d with keep %v, and max generation 3", id, profile, before, assetID, keep)
	}

	requestKeep := []chapters.Range{{StartMs: 5000, EndMs: 6000}}
	keepAfter := []chapters.Range{{StartMs: 7000, EndMs: 8000}}
	interleave, calls := interleaveWrite(t, func() {
		bg := context.Background()
		f.SetEncodedAssetRebuildRequest(t, id, profile, requestKeep)
		if err := f.Chapters.Reset(bg, id); err != nil {
			t.Errorf("resetting chapters for recording %d during the read: %v", id, err)
		}
		f.SetAssetState(t, assetID, "deleting")
		f.SetMediaAssetKeepRanges(t, assetID, keepAfter)
		f.AddTombstoneCopy(t, assetID, "takamatsu", fmt.Sprintf("sites/home/%d/cut-takamatsu.mp4", id), 7)
		f.SetEncodeProfiles(t, id, nil)
	})
	got, err := f.NewInterleavedReconcile(t, interleave).GetEncodedPublishState(ctx, id, profile, true, true)
	if n := calls.Load(); n != 1 {
		t.Fatalf("interleaved writes ran %d times, want 1", n)
	}
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q) across the writes: %v", id, profile, err)
	}
	if diff := encodedPublishStateDiff(before, got); len(diff) != 0 {
		t.Fatalf("GetEncodedPublishState(%d, %q) across the writes differs from the pre-write state in %v: got %+v, want %+v", id, profile, diff, got, before)
	}

	// 割り込みが各項目を実際に動かしたことの確認。desired を外すと章の項目は読まれなくなるので、
	// desired を外した後の読みでは desired だけを確かめ、章・作り直し要求・encoded などは desired を戻した後に確かめる。
	afterWrites, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, true)
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q) after the writes: %v", id, profile, err)
	}
	if diff := encodedPublishStateDiff(before, afterWrites); !containsAll(diff, "Desired") {
		t.Fatalf("GetEncodedPublishState(%d, %q) after the writes changed %v; want Desired to change", id, profile, diff)
	}
	f.SetEncodeProfiles(t, id, []string{profile})
	restored, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, true)
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q) after restoring desired: %v", id, profile, err)
	}
	if diff := encodedPublishStateDiff(before, restored); !containsAll(diff, encodedPublishStateFields[1:]...) {
		t.Fatalf("GetEncodedPublishState(%d, %q) after the writes changed %v; want every field except Desired to change", id, profile, diff)
	}
}

// containsAll は got が want の全ての名前を含むかを返す。
func containsAll(got []string, want ...string) bool {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
