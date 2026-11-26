package contracttest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

// RunIngestCheckpoint は録画の writer lease・checkpoint・進捗の契約を実行する。
// 再開・世代の進行・破棄・旧 writer の締め出しを、2 つの方言で同じ主張として検査する。
func RunIngestCheckpoint(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("clean handoff resumes and fences a superseded writer", func(t *testing.T) { runIngestCleanHandoff(t, newFixture(t)) })
	t.Run("discard never reuses a writer generation", func(t *testing.T) { runIngestDiscardGeneration(t, newFixture(t)) })
	t.Run("visible progress stays with the old writer until the new one catches up", func(t *testing.T) {
		runIngestProgressCatchUp(t, newFixture(t))
	})
}

func runIngestCleanHandoff(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	firstOwner := uuid.New()
	checkpoint, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, firstOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first Acquire = (%+v, %t, %v), want acquired", checkpoint, acquired, err)
	}
	if checkpoint.WriterGeneration != 1 || checkpoint.Resumed || checkpoint.Offset != 0 {
		t.Fatalf("first checkpoint = %+v, want fresh writer generation 1", checkpoint)
	}
	if _, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("second Acquire during live lease = (acquired=%t, %v), want busy", acquired, err)
	}

	hashState := []byte("sha256-state-at-offset")
	if err := f.Publisher.SaveIngestCheckpoint(ctx, recordingID, firstOwner, 1, 188, hashState); err != nil {
		t.Fatalf("Save handoff: %v", err)
	}
	secondOwner := uuid.New()
	resumed, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, secondOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after clean handoff = (%+v, %t, %v), want acquired", resumed, acquired, err)
	}
	if !resumed.Resumed || resumed.WriterGeneration != 1 || resumed.Offset != 188 || !bytes.Equal(resumed.HashState, hashState) {
		t.Fatalf("resumed checkpoint = %+v, want generation 1, offset 188 and saved hash state", resumed)
	}

	newGeneration, err := f.Publisher.ResetIngestCheckpoint(ctx, recordingID, secondOwner, resumed.WriterGeneration)
	if err != nil || newGeneration != 2 {
		t.Fatalf("Reset checkpoint = (%d, %v), want generation 2", newGeneration, err)
	}
	if err := f.Publisher.SaveIngestCheckpoint(ctx, recordingID, secondOwner, 1, 200, []byte("stale")); !errors.Is(err, store.ErrIngestCheckpointLeaseLost) {
		t.Fatalf("saving with superseded writer generation = %v, want lease lost", err)
	}
}

// 破棄のあとに始まる writer は、破棄した世代を再利用しない。temp 名に世代が入るので、
// 再利用すると残っている古い temp と衝突する。
func runIngestDiscardGeneration(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	owner := uuid.New()
	if _, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if err := f.Publisher.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 10, TempRelPath: "sites/default/.rokuban-ingest-default-record-g1", TempGeneration: 5,
	}, owner, 1, time.Minute); err != nil {
		t.Fatalf("writing progress: %v", err)
	}
	if discarded, err := f.Publisher.DiscardIngestCheckpoint(ctx, recordingID, owner, 1); err != nil || !discarded {
		t.Fatalf("Discard = (%t, %v), want discarded", discarded, err)
	}
	if state, err := f.Publisher.GetIngestTempState(ctx, recordingID); err != nil || state != nil {
		t.Fatalf("ingest temp state after discard = (%+v, %v), want none", state, err)
	}
	next, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after discard = (%+v, %t, %v), want acquired", next, acquired, err)
	}
	if next.WriterGeneration <= 1 || next.Resumed || next.Offset != 0 {
		t.Fatalf("writer after discard = %+v, want a fresh generation above the discarded 1", next)
	}
}

func runIngestProgressCatchUp(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	oldOwner := uuid.New()
	if _, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, oldOwner, time.Minute); err != nil || !acquired {
		t.Fatalf("old writer Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	oldPath := "sites/default/.rokuban-ingest-default-record-g1"
	const byteGeneration int64 = 73
	if err := f.Publisher.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 100, TempRelPath: oldPath, TempGeneration: byteGeneration,
	}, oldOwner, 1, time.Minute); err != nil {
		t.Fatalf("writing old visible progress: %v", err)
	}
	f.ExpireIngestLease(t, recordingID)
	newOwner := uuid.New()
	checkpoint, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, newOwner, time.Minute)
	if err != nil || !acquired || checkpoint.WriterGeneration != 2 {
		t.Fatalf("new writer Acquire = (%+v, %t, %v), want generation 2", checkpoint, acquired, err)
	}
	newPath := "sites/default/.rokuban-ingest-default-record-g2"
	writeProgress := func(written int64) error {
		return f.Publisher.UpdateIngestProgress(ctx, store.IngestProgress{
			RecordingID: recordingID, WrittenBytes: written, TempRelPath: newPath, TempGeneration: byteGeneration,
		}, newOwner, checkpoint.WriterGeneration, time.Minute)
	}
	for _, written := range []int64{0, 99} {
		if err := writeProgress(written); err != nil {
			t.Fatalf("writing new progress at %d: %v", written, err)
		}
		requireVisibleIngestProgress(t, f, recordingID, oldPath, 100, byteGeneration, "while the new writer is below the old offset")
	}
	if err := writeProgress(100); err != nil {
		t.Fatalf("catching up new writer: %v", err)
	}
	requireVisibleIngestProgress(t, f, recordingID, newPath, 100, byteGeneration, "after the new writer caught up")
	if err := f.Publisher.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 101, TempRelPath: oldPath, TempGeneration: byteGeneration,
	}, oldOwner, 1, time.Minute); !errors.Is(err, store.ErrIngestCheckpointLeaseLost) {
		t.Fatalf("zombie progress update = %v, want lease lost", err)
	}
}

// requireVisibleIngestProgress は録画の進捗行の見えている temp パス・書き込み量・世代を検査する。
func requireVisibleIngestProgress(t *testing.T, f MediaFixture, recordingID int64, wantPath string, wantBytes, wantGeneration int64, when string) {
	t.Helper()
	state, err := f.Publisher.GetIngestTempState(t.Context(), recordingID)
	if err != nil || state == nil {
		t.Fatalf("GetIngestTempState %s = (%+v, %v), want a row", when, state, err)
	}
	written, _ := mediaIngestProgress(t, f, recordingID)
	if state.TempRelPath != wantPath || written == nil || *written != wantBytes || state.TempGeneration != wantGeneration {
		t.Fatalf("visible progress %s = (%q, %s, %d), want (%q, %d, %d)", when, state.TempRelPath, ptrText(written), state.TempGeneration, wantPath, wantBytes, wantGeneration)
	}
}
