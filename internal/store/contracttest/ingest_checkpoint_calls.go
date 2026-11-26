package contracttest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

// RunIngestCheckpointCalls は、取り込みの呼び出し元が頼る残りの checkpoint の主張を検査する。
// 同じ値の再保存が lease 喪失にならないこと（MySQL は値が変わらない更新の影響行数を 0 と数えるため）を含む。
func RunIngestCheckpointCalls(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("canonical rel_path is reserved once per checkpoint", func(t *testing.T) { runIngestReserveRelPath(t, newFixture(t)) })
	t.Run("final handoff can be saved again with the same values", func(t *testing.T) { runIngestFinalHandoffRetry(t, newFixture(t)) })
	t.Run("released lease lets the next writer resume a clean handoff", func(t *testing.T) { runIngestRelease(t, newFixture(t)) })
	t.Run("temp generations strictly increase", func(t *testing.T) { runIngestTempGenerations(t, newFixture(t)) })
	t.Run("upserted progress is visible without a lease", func(t *testing.T) { runIngestUpsertProgress(t, newFixture(t)) })
}

func runIngestReserveRelPath(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	owner := uuid.New()
	if _, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	first := "sites/default/first-candidate.m2ts"
	got, err := f.Publisher.ReserveIngestRelPath(ctx, recordingID, owner, first)
	if err != nil || got != first {
		t.Fatalf("first ReserveIngestRelPath = (%q, %v), want %q", got, err, first)
	}
	got, err = f.Publisher.ReserveIngestRelPath(ctx, recordingID, owner, "sites/default/second-candidate.m2ts")
	if err != nil || got != first {
		t.Fatalf("second ReserveIngestRelPath = (%q, %v), want the first reservation %q", got, err, first)
	}
	if _, err := f.Publisher.ReserveIngestRelPath(ctx, recordingID, uuid.New(), "sites/default/other.m2ts"); !errors.Is(err, store.ErrIngestCheckpointLeaseLost) {
		t.Fatalf("ReserveIngestRelPath by a non-owner = %v, want lease lost", err)
	}
}

func runIngestFinalHandoffRetry(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	owner := uuid.New()
	checkpoint, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, owner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire = (%+v, %t, %v), want acquired", checkpoint, acquired, err)
	}
	hash := []byte("final-hash-state")
	for i := range 2 {
		if err := f.Publisher.SaveIngestFinalHandoff(ctx, recordingID, owner, checkpoint.WriterGeneration, 512, hash); err != nil {
			t.Fatalf("SaveIngestFinalHandoff attempt %d with the same values = %v, want nil", i+1, err)
		}
	}
	if err := f.Publisher.SaveIngestFinalHandoff(ctx, recordingID, uuid.New(), checkpoint.WriterGeneration, 512, hash); !errors.Is(err, store.ErrIngestCheckpointLeaseLost) {
		t.Fatalf("SaveIngestFinalHandoff by a non-owner = %v, want lease lost", err)
	}
	f.ExpireIngestLease(t, recordingID)
	resumed, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute)
	if err != nil || !acquired || !resumed.Resumed || resumed.Offset != 512 || !bytes.Equal(resumed.HashState, hash) {
		t.Fatalf("Acquire after the final handoff = (%+v, %t, %v), want resumed at 512 with the saved hash", resumed, acquired, err)
	}
}

func runIngestRelease(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	owner := uuid.New()
	if _, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	hash := []byte("released-hash")
	// 最終 handoff は lease を残すので、release の前は次の writer が入れない。
	if err := f.Publisher.SaveIngestFinalHandoff(ctx, recordingID, owner, 1, 64, hash); err != nil {
		t.Fatalf("SaveIngestFinalHandoff: %v", err)
	}
	if _, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("Acquire while the handed-off lease is live = (acquired=%t, %v), want busy", acquired, err)
	}
	if err := f.Publisher.ReleaseIngestCheckpoint(ctx, recordingID, owner); err != nil {
		t.Fatalf("ReleaseIngestCheckpoint: %v", err)
	}
	resumed, acquired, err := f.Publisher.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute)
	if err != nil || !acquired || !resumed.Resumed || resumed.WriterGeneration != 1 || resumed.Offset != 64 || !bytes.Equal(resumed.HashState, hash) {
		t.Fatalf("Acquire right after release = (%+v, %t, %v), want resumed at generation 1 offset 64", resumed, acquired, err)
	}
}

func runIngestTempGenerations(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	previous := int64(0)
	for i := range 3 {
		generation, err := f.Publisher.NextIngestTempGeneration(ctx)
		if err != nil || generation <= previous {
			t.Fatalf("NextIngestTempGeneration #%d = (%d, %v), want a positive value above %d", i+1, generation, err, previous)
		}
		previous = generation
	}
}

func runIngestUpsertProgress(t *testing.T, f MediaFixture) {
	ctx := t.Context()
	recordingID := f.NewRecording(t)
	expected := int64(4096)
	if err := f.Publisher.UpsertIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 0, ExpectedBytes: &expected,
		TempRelPath: "sites/default/.rokuban-ingest-upsert", TempGeneration: 7,
	}); err != nil {
		t.Fatalf("UpsertIngestProgress(0 bytes): %v", err)
	}
	state, err := f.Publisher.GetIngestTempState(ctx, recordingID)
	if err != nil || state == nil || state.TempRelPath != "sites/default/.rokuban-ingest-upsert" || state.TempGeneration != 7 {
		t.Fatalf("GetIngestTempState after the first write = (%+v, %v), want the written temp at generation 7", state, err)
	}
	written, gotExpected := mediaIngestProgress(t, f, recordingID)
	if written == nil || *written != 0 || gotExpected == nil || *gotExpected != expected {
		t.Fatalf("RecordingQuery progress after the first write = (%s, %s), want 0/%d", ptrText(written), ptrText(gotExpected), expected)
	}
	// 2 回目の書き込みは、同じ行の temp と世代と量を置き換える（行の存在のまま上書きされる）。
	if err := f.Publisher.UpsertIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 5, ExpectedBytes: &expected,
		TempRelPath: "sites/default/.rokuban-ingest-upsert-2", TempGeneration: 8,
	}); err != nil {
		t.Fatalf("UpsertIngestProgress(second write): %v", err)
	}
	state, err = f.Publisher.GetIngestTempState(ctx, recordingID)
	if err != nil || state == nil || state.TempRelPath != "sites/default/.rokuban-ingest-upsert-2" || state.TempGeneration != 8 {
		t.Fatalf("GetIngestTempState after the second write = (%+v, %v), want the overwritten temp at generation 8", state, err)
	}
	written, _ = mediaIngestProgress(t, f, recordingID)
	if written == nil || *written != 5 {
		t.Fatalf("RecordingQuery written bytes after the second write = %s, want 5", ptrText(written))
	}
}
