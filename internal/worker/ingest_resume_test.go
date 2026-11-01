package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

func TestIngestTempFilePathEscapesIdentityAndIncludesWriterGeneration(t *testing.T) {
	dir := t.TempDir()
	path := ingestTempFilePath(dir, "site-a", "record/../other")
	if filepath.Dir(path) != dir {
		t.Fatalf("temp path escaped directory: %q", path)
	}
	if got, want := filepath.Base(path), ".rokuban-ingest-site%2Da-record%2F..%2Fother-g1"; got != want {
		t.Fatalf("temp basename = %q, want %q", got, want)
	}
	if other := ingestTempFilePath(dir, "site-a", "record%2F..%2Fother"); filepath.Base(other) == filepath.Base(path) {
		t.Fatalf("temp basename collision for escaped record IDs: %q", filepath.Base(path))
	}
	if got := filepath.Base(ingestTempWriterFilePath(dir, "site-a", "record/../other", 2)); !strings.HasSuffix(got, "-g2") {
		t.Fatalf("second writer path = %q, want writer generation 2", got)
	}
}

// Cancellation leaves an unclean handoff. The retry must use Range 0 and a new
// writer file while the streamer-facing path continues to point at the old bytes
// until the new writer catches up, then removes the superseded temp on commit.
// Writer generation must not change the HLS byte generation.
//
//nolint:funlen // This integration test follows one restart through the visible pointer, catch-up, and final commit.
func TestIngestWorker_UncleanRestartUsesRangeZeroAndKeepsVisibleTemp(t *testing.T) {
	full := makeTSData(8)
	cutoff := 3 * 188

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var firstStream atomic.Bool
	var retryRange atomic.Int64
	retryRange.Store(-1)
	var deleteRequested atomic.Bool
	firstChunkFlushed := make(chan struct{}, 1)
	retryStreamStarted := make(chan struct{}, 1)
	retryStreamRelease := make(chan struct{})
	var retryStreamReleaseOnce sync.Once
	releaseRetryStream := func() { retryStreamReleaseOnce.Do(func() { close(retryStreamRelease) }) }
	t.Cleanup(releaseRetryStream)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			offset := mirakctest.RangeOffset(r)
			if offset == 0 && firstStream.CompareAndSwap(false, true) {
				// Keep the response open after the prefix. The test cancels Work only
				// after recording_ingest_progress confirms that the temp Write completed.
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(full[:cutoff])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				firstChunkFlushed <- struct{}{}
				<-r.Context().Done()
				return
			}
			if offset == 0 {
				retryRange.Store(offset)
				select {
				case retryStreamStarted <- struct{}{}:
				default:
				}
				<-retryStreamRelease
			}
			mirakctest.WriteRecordStream(w, r, full)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "finished", Options: mirakc.Options{ContentPath: strPtr("resume/recording.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/resume/recording.m2ts", Sha256: strPtr(sha256Hex(full))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			deleteRequested.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	mediaDir := t.TempDir()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-resume")
	w := &IngestWorker{
		Publisher: pgstoretest.Media(pool), MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		RecordTarget: pgstoretest.Recordings(pool), MediaDir: mediaDir, StallTimeout: 5 * time.Second,
		Precheck: pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	args := IngestJobArgs{Site: "default", RecordID: "rec-resume"}
	firstWork := make(chan error, 1)
	go func() { firstWork <- runWorkerViaJobQFake(ctx, w, args) }()
	select {
	case <-firstChunkFlushed:
	case <-time.After(5 * time.Second):
		t.Fatal("first stream did not flush its partial prefix")
	}
	progressDeadline := time.Now().Add(5 * time.Second)
	for {
		var writtenBytes int64
		err := pool.QueryRow(context.Background(), `SELECT written_bytes FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&writtenBytes)
		if err == nil && writtenBytes == int64(cutoff) {
			break
		}
		if time.Now().After(progressDeadline) {
			t.Fatalf("visible progress after first file Write = (%d, %v), want %d bytes", writtenBytes, err, cutoff)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-firstWork; err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("first Work() error = %v, want context.Canceled", err)
	}

	fullPath := filepath.Join(mediaDir, "sites", "default", "resume", "recording.m2ts")
	firstTemp := ingestTempWriterFilePath(filepath.Dir(fullPath), args.Site, args.RecordID, 1)
	info, err := os.Stat(firstTemp)
	if err != nil || info.Size() != int64(cutoff) {
		t.Fatalf("unclean writer temp stat = (%v, %v), want %d bytes", info, err, cutoff)
	}
	var firstByteGeneration, visibleBytes int64
	var visiblePath string
	if err := pool.QueryRow(context.Background(), `SELECT temp_generation, written_bytes, temp_rel_path FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&firstByteGeneration, &visibleBytes, &visiblePath); err != nil {
		t.Fatal(err)
	}
	firstTempRelPath, err := ingestTempRelPath(mediaDir, firstTemp)
	if err != nil {
		t.Fatal(err)
	}
	if visiblePath != firstTempRelPath {
		t.Fatalf("visible temp path after cancellation = %q, want the first writer path %q", visiblePath, firstTempRelPath)
	}
	if visibleBytes != int64(cutoff) {
		t.Fatalf("visible bytes after cancellation = %d, want %d", visibleBytes, cutoff)
	}

	secondTemp := ingestTempWriterFilePath(filepath.Dir(fullPath), args.Site, args.RecordID, 2)
	retryDone := make(chan error, 1)
	go func() { retryDone <- runWorkerViaJobQFake(context.Background(), w, args) }()
	select {
	case <-retryStreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not reach the gated Range 0 stream")
	}
	if info, err := os.Stat(secondTemp); err != nil || info.Size() != 0 {
		t.Fatalf("new writer temp before catch-up = (%v, %v), want empty generation 2", info, err)
	}
	var pathBeforeCatchup string
	var bytesBeforeCatchup, generationBeforeCatchup int64
	if err := pool.QueryRow(context.Background(), `SELECT temp_rel_path, written_bytes, temp_generation FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&pathBeforeCatchup, &bytesBeforeCatchup, &generationBeforeCatchup); err != nil {
		t.Fatal(err)
	}
	if pathBeforeCatchup != firstTempRelPath || bytesBeforeCatchup != int64(cutoff) || generationBeforeCatchup != firstByteGeneration {
		t.Fatalf("visible progress before catch-up = (%q, %d, %d), want old pointer/offset/generation (%q, %d, %d)", pathBeforeCatchup, bytesBeforeCatchup, generationBeforeCatchup, firstTempRelPath, cutoff, firstByteGeneration)
	}
	releaseRetryStream()
	if err := <-retryDone; err != nil {
		t.Fatalf("retry Work() error: %v", err)
	}
	if _, err := os.Stat(secondTemp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("new writer temp after commit: err=%v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(firstTemp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("superseded writer temp after commit: err=%v, want os.ErrNotExist", err)
	}
	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading committed original: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("committed original differs: got %d bytes, want %d", len(got), len(full))
	}
	if got := retryRange.Load(); got != 0 {
		t.Errorf("retry Range offset = %d, want Range 0 after an unclean handoff", got)
	}
	var committedByteGeneration int64
	if err := pool.QueryRow(context.Background(), `SELECT ingest_generation FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, recordingID).Scan(&committedByteGeneration); err != nil {
		t.Fatal(err)
	}
	if committedByteGeneration != firstByteGeneration {
		t.Errorf("committed byte generation = %d, want the unchanged HLS generation %d after writer catch-up", committedByteGeneration, firstByteGeneration)
	}
	if !deleteRequested.Load() {
		t.Error("edge record was not deleted after commit")
	}
}

func TestIngestWorker_RejectsSymlinkedCleanHandoff(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	media := pgstoretest.Media(pool)
	recordingID := dbtest.Recording(t, pool)
	prefix := []byte("already-written-prefix")
	hasher := sha256.New()
	if _, err := hasher.Write(prefix); err != nil {
		t.Fatal(err)
	}
	hashState, err := fsink.MarshalHashState(hasher)
	if err != nil {
		t.Fatalf("marshaling checkpoint hash state: %v", err)
	}
	firstOwner := uuid.New()
	checkpoint, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, firstOwner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first Acquire = (%+v, %t, %v), want acquired", checkpoint, acquired, err)
	}
	mediaDir := t.TempDir()
	dir := filepath.Join(mediaDir, "sites", "default", "test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tempPath := ingestTempWriterFilePath(dir, "default", "rec-symlink", checkpoint.WriterGeneration)
	externalPath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(externalPath, prefix, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalPath, tempPath); err != nil {
		t.Fatal(err)
	}
	if err := media.SaveIngestCheckpoint(ctx, recordingID, firstOwner, checkpoint.WriterGeneration, int64(len(prefix)), hashState); err != nil {
		t.Fatalf("saving clean handoff: %v", err)
	}
	secondOwner := uuid.New()
	checkpoint, acquired, err = media.AcquireIngestCheckpoint(ctx, recordingID, secondOwner, time.Minute)
	if err != nil || !acquired || !checkpoint.Resumed {
		t.Fatalf("second Acquire = (%+v, %t, %v), want resumed handoff", checkpoint, acquired, err)
	}

	w := &IngestWorker{MediaDir: mediaDir, Publisher: media}
	checkpoint, openedPath, _, file, offset, err := w.openIngestWriterFile(
		ctx, dir, "default", "rec-symlink", recordingID, secondOwner, checkpoint,
	)
	if err != nil {
		t.Fatalf("opening replacement writer: %v", err)
	}
	defer func() { _ = file.Close() }()
	if checkpoint.WriterGeneration != 2 || checkpoint.Resumed || offset != 0 {
		t.Fatalf("checkpoint after rejecting symlink = (%+v, offset %d), want fresh generation 2 at zero", checkpoint, offset)
	}
	if openedPath != ingestTempWriterFilePath(dir, "default", "rec-symlink", 2) {
		t.Fatalf("replacement path = %q, want generation 2", openedPath)
	}
	if got, err := os.ReadFile(externalPath); err != nil || !bytes.Equal(got, prefix) {
		t.Fatalf("symlink target after rejecting handoff = (%q, %v), want unchanged prefix", got, err)
	}
	if info, err := os.Lstat(tempPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("original handoff path = (%v, %v), want preserved symlink", info, err)
	}
}
