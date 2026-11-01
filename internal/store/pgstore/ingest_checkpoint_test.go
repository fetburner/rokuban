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

func TestIngestCheckpointCleanHandoffResumesAndFencesZombie(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	firstOwner := uuid.New()
	checkpoint, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, firstOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first Acquire = (%+v, %t, %v), want acquired", checkpoint, acquired, err)
	}
	if checkpoint.WriterGeneration != 1 || checkpoint.Resumed || checkpoint.Offset != 0 {
		t.Fatalf("first checkpoint = %+v, want fresh writer generation 1", checkpoint)
	}
	if _, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute); err != nil || acquired {
		t.Fatalf("second Acquire during live lease = (acquired=%t, %v), want busy", acquired, err)
	}

	hashState := []byte("sha256-state-at-offset")
	if err := media.SaveIngestCheckpoint(ctx, recordingID, firstOwner, 1, 188, hashState); err != nil {
		t.Fatalf("Save handoff: %v", err)
	}
	secondOwner := uuid.New()
	resumed, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, secondOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after clean handoff = (%+v, %t, %v), want acquired", resumed, acquired, err)
	}
	if !resumed.Resumed || resumed.WriterGeneration != 1 || resumed.Offset != 188 || !bytes.Equal(resumed.HashState, hashState) {
		t.Fatalf("resumed checkpoint = %+v, want generation 1, offset 188 and saved hash state", resumed)
	}

	newGeneration, err := media.ResetIngestCheckpoint(ctx, recordingID, secondOwner, resumed.WriterGeneration)
	if err != nil || newGeneration != 2 {
		t.Fatalf("Reset checkpoint = (%d, %v), want generation 2", newGeneration, err)
	}
	if err := media.SaveIngestCheckpoint(ctx, recordingID, secondOwner, 1, 200, []byte("stale")); !errors.Is(err, store.ErrIngestCheckpointLeaseLost) {
		t.Fatalf("saving with superseded writer generation = %v, want lease lost", err)
	}
}

// 破棄のあとに始まる writer は、破棄した世代を再利用しない。temp 名に世代が入るので、
// 再利用すると残っている古い temp と衝突する。
func TestIngestCheckpointDiscardNeverReusesWriterGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	owner := uuid.New()
	if _, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, owner, time.Minute); err != nil || !acquired {
		t.Fatalf("first Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	if err := media.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 10, TempRelPath: "sites/default/.rokuban-ingest-default-record-g1", TempGeneration: 5,
	}, owner, 1, time.Minute); err != nil {
		t.Fatalf("writing progress: %v", err)
	}
	if discarded, err := media.DiscardIngestCheckpoint(ctx, recordingID, owner, 1); err != nil || !discarded {
		t.Fatalf("Discard = (%t, %v), want discarded", discarded, err)
	}
	var progressRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&progressRows); err != nil {
		t.Fatal(err)
	}
	if progressRows != 0 {
		t.Fatalf("progress rows after discard = %d, want 0", progressRows)
	}
	next, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, uuid.New(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("Acquire after discard = (%+v, %t, %v), want acquired", next, acquired, err)
	}
	if next.WriterGeneration <= 1 || next.Resumed || next.Offset != 0 {
		t.Fatalf("writer after discard = %+v, want a fresh generation above the discarded 1", next)
	}
}

func TestIngestProgressKeepsVisibleWriterUntilNewWriterCatchesUp(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	oldOwner := uuid.New()
	if _, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, oldOwner, time.Minute); err != nil || !acquired {
		t.Fatalf("old writer Acquire = (acquired=%t, %v), want acquired", acquired, err)
	}
	oldPath := "sites/default/.rokuban-ingest-default-record-g1"
	const byteGeneration int64 = 73
	if err := media.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 100, TempRelPath: oldPath, TempGeneration: byteGeneration,
	}, oldOwner, 1, time.Minute); err != nil {
		t.Fatalf("writing old visible progress: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recording_ingest_checkpoints SET lease_expires_at = now() - interval '1 second' WHERE recording_id = $1`, recordingID); err != nil {
		t.Fatalf("expiring old writer lease: %v", err)
	}
	newOwner := uuid.New()
	checkpoint, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, newOwner, time.Minute)
	if err != nil || !acquired || checkpoint.WriterGeneration != 2 {
		t.Fatalf("new writer Acquire = (%+v, %t, %v), want generation 2", checkpoint, acquired, err)
	}
	newPath := "sites/default/.rokuban-ingest-default-record-g2"
	writeProgress := func(written int64) error {
		return media.UpdateIngestProgress(ctx, store.IngestProgress{
			RecordingID: recordingID, WrittenBytes: written, TempRelPath: newPath, TempGeneration: byteGeneration,
		}, newOwner, checkpoint.WriterGeneration, time.Minute)
	}
	for _, written := range []int64{0, 99} {
		if err := writeProgress(written); err != nil {
			t.Fatalf("writing new progress at %d: %v", written, err)
		}
		path, bytes, generation := readIngestProgressRow(t, pool, recordingID)
		if path != oldPath || bytes != 100 || generation != byteGeneration {
			t.Fatalf("visible progress at new writer offset %d = (%q, %d, %d), want old pointer/bytes/generation (%q, 100, %d)", written, path, bytes, generation, oldPath, byteGeneration)
		}
	}
	if err := writeProgress(100); err != nil {
		t.Fatalf("catching up new writer: %v", err)
	}
	path, bytes, generation := readIngestProgressRow(t, pool, recordingID)
	if path != newPath || bytes != 100 || generation != byteGeneration {
		t.Fatalf("visible progress after catch-up = (%q, %d, %d), want new pointer with unchanged byte generation (%q, 100, %d)", path, bytes, generation, newPath, byteGeneration)
	}
	if err := media.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 101, TempRelPath: oldPath, TempGeneration: byteGeneration,
	}, oldOwner, 1, time.Minute); !errors.Is(err, store.ErrIngestCheckpointLeaseLost) {
		t.Fatalf("zombie progress update = %v, want lease lost", err)
	}
}

func readIngestProgressRow(t *testing.T, pool *pgxpool.Pool, recordingID int64) (path string, bytesWritten, generation int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT temp_rel_path, written_bytes, temp_generation
FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&path, &bytesWritten, &generation); err != nil {
		t.Fatalf("reading visible ingest progress: %v", err)
	}
	return path, bytesWritten, generation
}
