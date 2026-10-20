package contracttest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// CMDetectionFixture は CM 検出契約と競合状態を作るためのフックをまとめる。
type CMDetectionFixture struct {
	Store          store.CMDetection
	ChapterStore   store.ChapterEdits
	CMStationStore store.CMStation
	TrashStore     store.RecordingTrash
	NewRecording   func(t *testing.T) int64
	// SeedLogo は検出結果の版照合を検証するため、局に学習済みロゴを作る。
	SeedLogo func(t *testing.T, station store.CMStationRef, recordingID int64)
	// BeginChapterWrite は録画行をロックしてチャプター編集を書き、commit 関数を返す。
	BeginChapterWrite func(t *testing.T, recordingID int64) (commit func())
	// BeginLogoAreaWrite は局ロックを保持して枠を更新し、commit 関数を返す。
	BeginLogoAreaWrite func(t *testing.T, station store.CMStationRef, input store.CMLogoAreaInput) (commit func())
	// WaitForRecordingWriteWait は name の操作が録画行ロックで待っていることを確認する。
	WaitForRecordingWriteWait func(t *testing.T, recordingID int64, name string)
	// WaitForStationWriteWait は name の操作が局ロックで待っていることを確認する。
	WaitForStationWriteWait func(t *testing.T, station store.CMStationRef, name string)
	// AdvancePolicyGeneration は現在の CM 検出世代を進める。
	AdvancePolicyGeneration func(t *testing.T, recordingID int64)
}

// NewCMDetectionStore は実在する CMDetection 契約と fixture を作る。
type NewCMDetectionStore func(t *testing.T) CMDetectionFixture

// RunCMDetection は実在する store 実装に CM 検出の契約を実行する。
func RunCMDetection(t *testing.T, newStore NewCMDetectionStore) {
	t.Helper()
	station := store.CMStationRef{NetworkID: 32678, ServiceID: 5168}

	t.Run("work item maps missing recording", func(t *testing.T) { testMissingCMDetectionWorkItem(t, newStore) })
	t.Run("attempt failure cancel and abort preserve their transitions", func(t *testing.T) {
		testCMDetectionAttemptTransitions(t, newStore, station)
	})
	t.Run("result ranges normalize and retry clears the observation", func(t *testing.T) {
		testCMDetectionResultRangesAndRetry(t, newStore)
	})
	t.Run("old generation cannot commit after a current attempt starts", func(t *testing.T) {
		testCMDetectionGenerationFence(t, newStore)
	})
	t.Run("trash during analysis clears the attempt budget", func(t *testing.T) {
		testCMDetectionTrashClearsAttempt(t, newStore)
	})
	t.Run("result rejects a logo learned after an attempt started without one", func(t *testing.T) {
		testCMDetectionRejectsNewLogo(t, newStore, station)
	})
	t.Run("result rejects a changed learned logo version", func(t *testing.T) {
		testCMDetectionRejectsChangedLogo(t, newStore, station)
	})
	t.Run("learned logo save returns the version used by the result", func(t *testing.T) {
		testCMDetectionLearnedLogoVersion(t, newStore, station)
	})

	runDetectionWaitsForChapterWrite(t, newStore)
	runLearnedLogoWaitsForAreaWrite(t, newStore, station)
}

func testCMDetectionTrashClearsAttempt(t *testing.T, newStore NewCMDetectionStore) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	var third store.CMDetectionAttempt
	for want := int32(1); want <= 3; want++ {
		attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
		if err != nil || !shouldRun || attempt.AttemptCount != want {
			t.Fatalf("BeginAttempt() = %+v, %v, %v; want running attempt %d", attempt, shouldRun, err, want)
		}
		third = attempt
		if want < 3 {
			marked, err := f.Store.FailAttempt(ctx, attempt, "retrying", nil, "temporary failure")
			if err != nil || !marked {
				t.Fatalf("FailAttempt(attempt %d) = %v, %v; want one updated row", want, marked, err)
			}
		}
	}

	if err := f.TrashStore.SoftDelete(ctx, id); err != nil {
		t.Fatalf("SoftDelete() error = %v", err)
	}
	if saved, err := f.Store.SaveDetection(ctx, third, []chapters.Range{{StartMs: 1000, EndMs: 2000}}); err != nil || saved {
		t.Fatalf("SaveDetection(trashed recording) = %v, %v; want discarded", saved, err)
	}
	if err := f.TrashStore.Restore(ctx, id); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	first, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun || first.AttemptCount != 1 {
		t.Fatalf("BeginAttempt() after restore = %+v, %v, %v; want a fresh running attempt 1", first, shouldRun, err)
	}
}

func testMissingCMDetectionWorkItem(t *testing.T, newStore NewCMDetectionStore) {
	t.Helper()
	f := newStore(t)
	if _, err := f.Store.GetWorkItem(context.Background(), -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetWorkItem(missing) error = %v, want ErrNotFound", err)
	}
}

func testCMDetectionAttemptTransitions(t *testing.T, newStore NewCMDetectionStore, station store.CMStationRef) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	item, err := f.Store.GetWorkItem(ctx, id)
	if err != nil {
		t.Fatalf("GetWorkItem() error = %v", err)
	}
	if item.ID != id || item.Station != station || !item.Enabled || item.RelPath == nil || item.IsTrashed || item.OriginalMissing || item.Detected {
		t.Fatalf("GetWorkItem() = %+v, want active original with CM detection enabled", item)
	}
	if desired, err := f.Store.IsDetectionDesired(ctx, id); err != nil || !desired {
		t.Fatalf("IsDetectionDesired() = %v, %v; want true", desired, err)
	}
	first, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun || first.AttemptCount != 1 || first.Station != station {
		t.Fatalf("BeginAttempt() = %+v, %v, %v; want running attempt 1", first, shouldRun, err)
	}
	marked, err := f.Store.FailAttempt(ctx, first, "retrying", nil, "temporary failure")
	if err != nil || !marked {
		t.Fatalf("FailAttempt() = %v, %v; want one updated row", marked, err)
	}
	second, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun || second.AttemptCount != 2 {
		t.Fatalf("second BeginAttempt() = %+v, %v, %v; want running attempt 2", second, shouldRun, err)
	}
	if err := f.Store.CancelAttempt(ctx, second); err != nil {
		t.Fatalf("CancelAttempt() error = %v", err)
	}
	retried, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun || retried.AttemptCount != 2 {
		t.Fatalf("BeginAttempt() after cancel = %+v, %v, %v; want running attempt 2", retried, shouldRun, err)
	}
	if err := f.Store.AbortAttempt(ctx, retried); err != nil {
		t.Fatalf("AbortAttempt() error = %v", err)
	}
	fresh, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun || fresh.AttemptCount != 1 {
		t.Fatalf("BeginAttempt() after abort = %+v, %v, %v; want a fresh attempt 1", fresh, shouldRun, err)
	}
}

func testCMDetectionResultRangesAndRetry(t *testing.T, newStore NewCMDetectionStore) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun {
		t.Fatalf("BeginAttempt() = %+v, %v, %v; want running", attempt, shouldRun, err)
	}
	ranges := []chapters.Range{{StartMs: 3000, EndMs: 4000}, {StartMs: 1000, EndMs: 3000}, {StartMs: 2000, EndMs: 5000}}
	saved, err := f.Store.SaveDetection(ctx, attempt, ranges)
	if err != nil || !saved {
		t.Fatalf("SaveDetection() = %v, %v; want saved", saved, err)
	}
	snapshot, err := f.ChapterStore.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading saved detection: %v", err)
	}
	wantRanges := []chapters.Range{{StartMs: 1000, EndMs: 5000}}
	if !snapshot.Detected || !reflect.DeepEqual(snapshot.CMRanges, wantRanges) {
		t.Fatalf("saved CM ranges = %+v (detected=%v), want canonical %v", snapshot.CMRanges, snapshot.Detected, wantRanges)
	}
	retry, err := f.Store.RetryDetection(ctx, id)
	if err != nil || !retry.ActiveOriginal || !retry.Desired || retry.RecordingDurationMs != 30*time.Minute.Milliseconds() {
		t.Fatalf("RetryDetection() = %+v, %v; want desired active original and 30-minute duration", retry, err)
	}
	snapshot, err = f.ChapterStore.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after retry: %v", err)
	}
	if snapshot.Detected || len(snapshot.CMRanges) != 0 {
		t.Fatalf("snapshot after retry = %+v, want no detection result", snapshot)
	}
}

func testCMDetectionGenerationFence(t *testing.T, newStore NewCMDetectionStore) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	old, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun {
		t.Fatalf("old BeginAttempt() = %+v, %v, %v; want running", old, shouldRun, err)
	}
	f.AdvancePolicyGeneration(t, id)
	current, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun || current.PolicyGeneration == old.PolicyGeneration {
		t.Fatalf("current BeginAttempt() = %+v, %v, %v; want a new running generation", current, shouldRun, err)
	}
	if saved, err := f.Store.SaveDetection(ctx, old, []chapters.Range{{StartMs: 1000, EndMs: 2000}}); err != nil || saved {
		t.Fatalf("SaveDetection(old generation) = %v, %v; want a discarded result", saved, err)
	}
	snapshot, err := f.ChapterStore.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after old generation result: %v", err)
	}
	if snapshot.Detected || len(snapshot.CMRanges) != 0 {
		t.Fatalf("old generation wrote detection = %+v, want no result", snapshot.CMRanges)
	}
	if saved, err := f.Store.SaveDetection(ctx, current, []chapters.Range{{StartMs: 1000, EndMs: 2000}}); err != nil || !saved {
		t.Fatalf("SaveDetection(current generation) = %v, %v; want saved", saved, err)
	}
}

func testCMDetectionRejectsNewLogo(t *testing.T, newStore NewCMDetectionStore, station store.CMStationRef) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun {
		t.Fatalf("BeginAttempt() = %+v, %v, %v; want running", attempt, shouldRun, err)
	}
	f.SeedLogo(t, station, id)
	if saved, err := f.Store.SaveDetection(ctx, attempt, []chapters.Range{{StartMs: 1000, EndMs: 2000}}); err != nil || saved {
		t.Fatalf("SaveDetection(after logo appeared) = %v, %v; want discarded", saved, err)
	}
	assertNoCMDetectionResult(t, f.ChapterStore, ctx, id, "result using the old logo")
}

func testCMDetectionRejectsChangedLogo(t *testing.T, newStore NewCMDetectionStore, station store.CMStationRef) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.SeedLogo(t, station, id)
	logo, err := f.Store.GetLogo(ctx, station)
	if err != nil {
		t.Fatalf("reading initial logo: %v", err)
	}
	attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun {
		t.Fatalf("BeginAttempt() = %+v, %v, %v; want running", attempt, shouldRun, err)
	}
	attempt.LogoLearnedAt = &logo.LearnedAt
	time.Sleep(time.Millisecond)
	f.SeedLogo(t, station, id)
	if saved, err := f.Store.SaveDetection(ctx, attempt, []chapters.Range{{StartMs: 1000, EndMs: 2000}}); err != nil || saved {
		t.Fatalf("SaveDetection(after logo changed) = %v, %v; want discarded", saved, err)
	}
	assertNoCMDetectionResult(t, f.ChapterStore, ctx, id, "result using an old logo version")
}

func assertNoCMDetectionResult(t *testing.T, chapterStore store.ChapterEdits, ctx context.Context, id int64, description string) {
	t.Helper()
	snapshot, err := chapterStore.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after stale logo result: %v", err)
	}
	if snapshot.Detected || len(snapshot.CMRanges) != 0 {
		t.Fatalf("%s was written = %+v, want no result", description, snapshot.CMRanges)
	}
}

func testCMDetectionLearnedLogoVersion(t *testing.T, newStore NewCMDetectionStore, station store.CMStationRef) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	areaInput := store.CMLogoAreaInput{RecordingID: id, X: 10, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080}
	area, err := f.CMStationStore.SaveLogoArea(ctx, station, areaInput)
	if err != nil {
		t.Fatalf("saving logo area: %v", err)
	}
	attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
	if err != nil || !shouldRun {
		t.Fatalf("BeginAttempt() = %+v, %v, %v; want running", attempt, shouldRun, err)
	}
	learnedAt, saved, err := f.Store.SaveLearnedLogo(ctx, attempt, store.CMDetectionLogo{
		LGD: []byte("learned logo"), CodedWidth: 1440, CodedHeight: 1080,
	}, &area.AreaUpdatedAt)
	if err != nil || !saved || learnedAt == nil {
		t.Fatalf("SaveLearnedLogo() = %v, %v, %v; want a saved logo version", learnedAt, saved, err)
	}
	attempt.LogoLearnedAt = learnedAt
	if saved, err := f.Store.SaveDetection(ctx, attempt, []chapters.Range{{StartMs: 1000, EndMs: 2000}}); err != nil || !saved {
		t.Fatalf("SaveDetection(after learned logo save) = %v, %v; want saved", saved, err)
	}
}

func runDetectionWaitsForChapterWrite(t *testing.T, newStore NewCMDetectionStore) {
	t.Helper()
	t.Run("result waits for chapter edit row lock", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		id := f.NewRecording(t)
		attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
		if err != nil || !shouldRun {
			t.Fatalf("BeginAttempt() = %+v, %v, %v; want running", attempt, shouldRun, err)
		}
		commit := f.BeginChapterWrite(t, id)
		committed := false
		defer func() {
			if !committed {
				commit()
			}
		}()
		result := make(chan struct {
			saved bool
			err   error
		}, 1)
		go func() {
			saved, err := f.Store.SaveDetection(ctx, attempt, []chapters.Range{{StartMs: 1000, EndMs: 2000}})
			result <- struct {
				saved bool
				err   error
			}{saved, err}
		}()
		f.WaitForRecordingWriteWait(t, id, "SaveDetection")
		commit()
		committed = true
		got := <-result
		if got.err != nil || !got.saved {
			t.Fatalf("SaveDetection() after chapter edit = %v, %v; want saved", got.saved, got.err)
		}
	})
}

func runLearnedLogoWaitsForAreaWrite(t *testing.T, newStore NewCMDetectionStore, station store.CMStationRef) {
	t.Helper()
	t.Run("learned logo waits and rejects the changed area version", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		id := f.NewRecording(t)
		areaInput := store.CMLogoAreaInput{RecordingID: id, X: 10, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080}
		if _, err := f.CMStationStore.SaveLogoArea(ctx, station, areaInput); err != nil {
			t.Fatalf("saving initial logo area: %v", err)
		}
		oldArea, err := f.Store.GetLogoArea(ctx, station)
		if err != nil {
			t.Fatalf("reading initial logo area: %v", err)
		}
		attempt, shouldRun, err := f.Store.BeginAttempt(ctx, id, 3)
		if err != nil || !shouldRun {
			t.Fatalf("BeginAttempt() = %+v, %v, %v; want running", attempt, shouldRun, err)
		}
		changedAreaInput := areaInput
		changedAreaInput.X = 20
		commit := f.BeginLogoAreaWrite(t, station, changedAreaInput)
		committed := false
		defer func() {
			if !committed {
				commit()
			}
		}()
		result := make(chan struct {
			saved     bool
			learnedAt *time.Time
			err       error
		}, 1)
		go func() {
			learnedAt, saved, err := f.Store.SaveLearnedLogo(ctx, attempt, store.CMDetectionLogo{
				LGD: []byte("learned logo"), CodedWidth: 1440, CodedHeight: 1080,
			}, &oldArea.UpdatedAt)
			result <- struct {
				saved     bool
				learnedAt *time.Time
				err       error
			}{saved: saved, learnedAt: learnedAt, err: err}
		}()
		f.WaitForStationWriteWait(t, station, "SaveLearnedLogo")
		commit()
		committed = true
		got := <-result
		if got.err != nil || got.saved || got.learnedAt != nil {
			t.Fatalf("SaveLearnedLogo(old area version) = %v, %v; want discarded", got.saved, got.err)
		}
		if _, err := f.Store.GetLogo(ctx, station); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetLogo() after stale learned logo = %v, want ErrNotFound", err)
		}
	})
}
