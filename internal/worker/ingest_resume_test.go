package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/mirakc"
)

// cancelAfterBytesTransport は response body の読み出し側で cutoff バイトを
// 返した直後に ctx を cancel する。サーバーの Flush はクライアントの temp への
// Write 完了を保証しないため、再開統合テストでは HTTP 層の受理位置ではなく
// io.Copy の直後を同期点にする。
type cancelAfterBytesTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
	cutoff int64
	done   atomic.Bool
}

func (t *cancelAfterBytesTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream") &&
		r.Header.Get("Range") == "bytes=0-" && t.done.CompareAndSwap(false, true) {
		resp.Body = &cancelAfterBytesBody{
			ReadCloser: resp.Body,
			remaining:  t.cutoff,
			cancel:     t.cancel,
		}
	}
	return resp, nil
}

type cancelAfterBytesBody struct {
	io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
}

func (r *cancelAfterBytesBody) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		r.cancel()
		return 0, context.Canceled
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining == 0 {
		// Return the cutoff bytes first. The next Read returns context.Canceled,
		// so the caller has already written exactly cutoff bytes to the temp.
		r.cancel()
	}
	return n, err
}

func TestCancelAfterBytesBodyCancelsAfterCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelAfterBytesBody{
		ReadCloser: io.NopCloser(strings.NewReader("abcdef")),
		remaining:  3,
		cancel:     cancel,
	}

	buf := make([]byte, 16)
	n, err := body.Read(buf)
	if err != nil {
		t.Fatalf("first read error = %v, want nil", err)
	}
	if n != 3 || string(buf[:n]) != "abc" {
		t.Fatalf("first read = (%d, %q), want (3, %q)", n, buf[:n], "abc")
	}
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("context after cutoff = %v, want context.Canceled", err)
	}
	if n, err := body.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cutoff = (%d, %v), want (0, context.Canceled)", n, err)
	}
}

// TestIngestTempFile_ResumeReplaysHash は、プロセス死を挟んだ再試行が temp の
// 先頭から SHA-256 を復元して末尾から続くことを固定する。
func TestIngestTempFile_ResumeReplaysHash(t *testing.T) {
	mediaDir := t.TempDir()
	tempPath := ingestTempFilePath(mediaDir, "site-a", "record-1")

	prefix := makeTSData(4)
	// 0,1,2,5 なので prefix 内に 1 drop を作る。
	prefix[3*188+3] = 0x10 | 5
	suffix := makeTSData(2)
	// prefix の最後が CC=5 なので、再開後は 6,7 と続ける。
	suffix[0*188+3] = 0x10 | 6
	suffix[1*188+3] = 0x10 | 7
	want := append(append([]byte(nil), prefix...), suffix...)

	firstLock, err := lockIngestTempFile(tempPath)
	if err != nil {
		t.Fatalf("locking first ingest temp: %v", err)
	}
	firstFile, err := openIngestFile(tempPath, firstLock)
	if err != nil {
		_ = firstLock.Close()
		t.Fatalf("opening first ingest temp: %v", err)
	}
	if _, err := firstFile.Write(prefix); err != nil {
		_ = firstFile.Close()
		_ = firstLock.Close()
		t.Fatalf("writing first ingest temp: %v", err)
	}
	if err := firstFile.Close(); err != nil {
		_ = firstLock.Close()
		t.Fatalf("closing first ingest temp: %v", err)
	}
	// プロセス死で fd が閉じた状態を、firstFile の close で模す。ロックと
	// writer は同じ open file description を共有するが fd は dup なので、
	// lock fd も閉じて排他を解放する。
	if err := firstLock.Close(); err != nil {
		t.Fatalf("releasing first ingest temp lock: %v", err)
	}

	secondLock, err := lockIngestTempFile(tempPath)
	if err != nil {
		t.Fatalf("locking resumed ingest temp: %v", err)
	}
	secondFile, err := openIngestFile(tempPath, secondLock)
	if err != nil {
		t.Fatalf("opening resumed ingest temp: %v", err)
	}
	defer func() {
		_ = secondFile.Close()
		_ = secondLock.Close()
	}()

	hasher := sha256.New()
	offset, err := replayIngestTempFile(context.Background(), tempPath, hasher)
	if err != nil {
		t.Fatalf("replaying ingest temp: %v", err)
	}
	if offset != int64(len(prefix)) {
		t.Fatalf("replayed offset = %d, want %d", offset, len(prefix))
	}

	sink := &hashingWriter{w: secondFile, h: hasher}
	if _, err := sink.Write(suffix); err != nil {
		t.Fatalf("writing resumed suffix: %v", err)
	}

	got, err := os.ReadFile(tempPath)
	if err != nil {
		t.Fatalf("reading resumed ingest temp: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("resumed temp bytes differ: got=%d want=%d", len(got), len(want))
	}
	if gotHash := hex.EncodeToString(hasher.Sum(nil)); gotHash != sha256Hex(want) {
		t.Errorf("replayed hash = %s, want %s", gotHash, sha256Hex(want))
	}
}

func TestIngestCheckpoint_ResumeFromTailMatchesSingleTransferHash(t *testing.T) {
	tempPath := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-1")
	prefix := []byte("committed prefix")
	suffix := []byte(" bytes after the checkpoint")

	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("creating ingest temp: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("closing ingest temp: %v", err)
		}
	}()
	if _, err := file.Write(prefix); err != nil {
		t.Fatalf("writing ingest prefix: %v", err)
	}
	prefixHash := sha256.New()
	_, _ = prefixHash.Write(prefix)
	if err := file.Sync(); err != nil {
		t.Fatalf("syncing ingest prefix: %v", err)
	}
	if err := writeIngestCheckpoint(tempPath, int64(len(prefix)), prefixHash); err != nil {
		t.Fatalf("writing ingest checkpoint: %v", err)
	}
	if _, err := file.Write(suffix); err != nil {
		t.Fatalf("writing bytes after checkpoint: %v", err)
	}
	checkpointOffset, restored := restoreIngestCheckpoint(tempPath, int64(len(prefix)+len(suffix)), sha256.New())
	if !restored || checkpointOffset != int64(len(prefix)) {
		t.Fatalf("restored checkpoint = (%d, %v), want (%d, true)", checkpointOffset, restored, len(prefix))
	}

	hasher := sha256.New()
	offset, complete, err := replayIngestTempFileWithCheckpoint(context.Background(), tempPath, file, hasher)
	if err != nil {
		t.Fatalf("replaying from checkpoint: %v", err)
	}
	if !complete {
		t.Fatal("replay did not complete")
	}
	if want := int64(len(prefix) + len(suffix)); offset != want {
		t.Fatalf("replayed offset = %d, want %d", offset, want)
	}
	if got, want := hex.EncodeToString(hasher.Sum(nil)), sha256Hex(append(bytes.Clone(prefix), suffix...)); got != want {
		t.Fatalf("resumed SHA-256 = %s, want single-transfer SHA-256 %s", got, want)
	}
}

func TestIngestCheckpoint_WriteDoesNotRefreshTempActivityTime(t *testing.T) {
	tempPath := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-1")
	if err := os.WriteFile(tempPath, []byte("committed prefix"), 0o644); err != nil {
		t.Fatalf("writing ingest temp: %v", err)
	}
	oldTime := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(tempPath, oldTime, oldTime); err != nil {
		t.Fatalf("setting ingest temp mtime: %v", err)
	}

	hasher := sha256.New()
	_, _ = hasher.Write([]byte("committed prefix"))
	if err := writeIngestCheckpoint(tempPath, int64(len("committed prefix")), hasher); err != nil {
		t.Fatalf("writing ingest checkpoint: %v", err)
	}

	info, err := os.Stat(tempPath)
	if err != nil {
		t.Fatalf("stating ingest temp: %v", err)
	}
	if !info.ModTime().Equal(oldTime) {
		t.Fatalf("ingest temp mtime = %s, want unchanged %s", info.ModTime(), oldTime)
	}
}

func TestIngestCheckpoint_InvalidCheckpointFallsBackToFullReplay(t *testing.T) {
	for _, checkpoint := range []string{"missing", "corrupt", "ahead of temp"} {
		t.Run(checkpoint, func(t *testing.T) {
			tempPath := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-1")
			data := []byte("complete transfer bytes")
			if err := os.WriteFile(tempPath, data, 0o644); err != nil {
				t.Fatalf("writing ingest temp: %v", err)
			}
			switch checkpoint {
			case "corrupt":
				if err := os.WriteFile(ingestCheckpointPath(tempPath), []byte("not a checkpoint"), 0o644); err != nil {
					t.Fatalf("writing corrupt checkpoint: %v", err)
				}
			case "ahead of temp":
				hasher := sha256.New()
				_, _ = hasher.Write(data)
				if err := writeIngestCheckpoint(tempPath, int64(len(data)+1), hasher); err != nil {
					t.Fatalf("writing ahead checkpoint: %v", err)
				}
			}

			file, err := os.OpenFile(tempPath, os.O_RDWR|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatalf("opening ingest temp: %v", err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Errorf("closing ingest temp: %v", err)
				}
			}()
			hasher := sha256.New()
			_, _ = hasher.Write([]byte("stale state"))
			offset, complete, err := replayIngestTempFileWithCheckpoint(context.Background(), tempPath, file, hasher)
			if err != nil {
				t.Fatalf("replaying with %s checkpoint: %v", checkpoint, err)
			}
			if !complete || offset != int64(len(data)) {
				t.Fatalf("replay = (offset %d, complete %v), want (%d, true)", offset, complete, len(data))
			}
			if got, want := hex.EncodeToString(hasher.Sum(nil)), sha256Hex(data); got != want {
				t.Fatalf("fallback SHA-256 = %s, want full-replay SHA-256 %s", got, want)
			}
		})
	}
}

func TestIngestReplayReaderHonorsCancellationBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &ingestReplayReader{ctx: ctx, r: strings.NewReader("abcdef")}
	buf := make([]byte, 3)

	n, err := reader.Read(buf)
	if err != nil || n != 3 || string(buf) != "abc" {
		t.Fatalf("first replay read = (%d, %v, %q), want (3, nil, %q)", n, err, buf, "abc")
	}
	cancel()
	if n, err := reader.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled replay read = (%d, %v), want (0, context.Canceled)", n, err)
	}
}

func TestIngestTempFile_LockIsNonBlockingAndReleasedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-1")

	first, err := lockIngestTempFile(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	second, err := lockIngestTempFile(path)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("second lock succeeded while first lock was held")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first lock: %v", err)
	}

	third, err := lockIngestTempFile(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("closing third lock: %v", err)
	}
}

func TestIngestTempFile_OrphanLockSkipsLiveIngestAndDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-1")

	live, err := lockIngestTempFile(path)
	if err != nil {
		t.Fatalf("live ingest lock: %v", err)
	}
	if _, err := lockExistingIngestTempFile(path); !ingestTempLockBusy(err) {
		t.Fatalf("orphan lock error = %v, want a non-blocking lock conflict", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("closing live ingest lock: %v", err)
	}

	missing := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-2")
	if _, err := lockExistingIngestTempFile(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing orphan lock error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing orphan lock created a file: stat error = %v", err)
	}
}

func TestIngestTempFile_WriterSharesLockedInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-site-a-record-1")
	lock, err := lockIngestTempFile(path)
	if err != nil {
		t.Fatalf("locking ingest temp: %v", err)
	}
	writer, err := openIngestFile(path, lock)
	if err != nil {
		_ = lock.Close()
		t.Fatalf("opening ingest writer: %v", err)
	}
	defer func() {
		_ = writer.Close()
		_ = lock.Close()
	}()

	writerFile, ok := writer.(*os.File)
	if !ok {
		t.Fatalf("writer type = %T, want *os.File", writer)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		t.Fatalf("stating lock fd: %v", err)
	}
	writerInfo, err := writerFile.Stat()
	if err != nil {
		t.Fatalf("stating writer fd: %v", err)
	}
	if !os.SameFile(lockInfo, writerInfo) {
		t.Fatal("writer and lock refer to different inodes")
	}
}

func TestIngestTempFilePath_DoesNotEscapeForSlashInRecordID(t *testing.T) {
	dir := t.TempDir()
	path := ingestTempFilePath(dir, "site-a", "record/../other")
	if filepath.Dir(path) != dir {
		t.Fatalf("temp path escaped directory: %q", path)
	}
	if filepath.Base(path) != ".rokuban-ingest-site-a-record%2F..%2Fother" {
		t.Fatalf("temp basename = %q, want escaped deterministic basename", filepath.Base(path))
	}
	if other := ingestTempFilePath(dir, "site-a", "record%2F..%2Fother"); filepath.Base(other) == filepath.Base(path) {
		t.Fatalf("temp basename collision for escaped record IDs: %q", filepath.Base(path))
	}
}

// TestIngestWorker_ResumesExistingTempAfterWorkCancellation は、Work が途中で
// 終了した後の新しい River job が、DB の進捗値ではなく同じ deterministic temp を
// replay して Range を末尾から再開することを固定する。
func TestIngestWorker_ResumesExistingTempAfterWorkCancellation(t *testing.T) {
	full := makeTSData(8)
	cutoff := 3 * 188

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var firstStream atomic.Bool
	var resumedAt atomic.Int64
	resumedAt.Store(-1)
	var deleteRequested atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			offset := parseStreamRangeOffset(r)
			if offset == int64(cutoff) {
				resumedAt.Store(offset)
			}
			if offset >= int64(len(full)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if offset == 0 && firstStream.CompareAndSwap(false, true) {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", cutoff))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(full[:cutoff])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				return
			}
			remaining := full[offset:]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(remaining)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Status:  "finished",
					Options: mirakc.Options{ContentPath: strPtr("resume/recording.m2ts")},
				},
				Content: mirakc.ContentInfo{
					Path:   "/recording/resume/recording.m2ts",
					Sha256: strPtr(sha256Hex(full)),
				},
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
	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-resume")
	streamClient := &http.Client{
		Transport: &cancelAfterBytesTransport{
			base:   http.DefaultTransport,
			cancel: cancel,
			cutoff: int64(cutoff),
		},
	}

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, streamClient)),
		Pool:          pool,
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}
	args := IngestJobArgs{Site: "default", RecordID: "rec-resume"}
	firstJob := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{ID: 847001},
		Args:   args,
	}
	if err := w.Work(ctx, firstJob); err == nil {
		t.Fatal("first Work() error = nil, want cancellation after partial transfer")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Work() error = %v, want context.Canceled", err)
	}

	fullPath := filepath.Join(mediaDir, "sites", "default", "resume", "recording.m2ts")
	tempPath := ingestTempFilePath(filepath.Dir(fullPath), "default", "rec-resume")
	info, err := os.Stat(tempPath)
	if err != nil {
		t.Fatalf("stat preserved ingest temp: %v", err)
	}
	if info.Size() != int64(cutoff) {
		t.Fatalf("preserved ingest temp size = %d, want %d", info.Size(), cutoff)
	}

	secondJob := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{ID: 847002},
		Args:   args,
	}
	if err := w.Work(context.Background(), secondJob); err != nil {
		t.Fatalf("resumed Work() error: %v", err)
	}

	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading resumed canonical file: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("resumed canonical bytes differ: got=%d want=%d", len(got), len(full))
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ingest temp after commit: err=%v, want os.ErrNotExist", err)
	}
	if got := resumedAt.Load(); got != int64(cutoff) {
		t.Errorf("resumed Range offset = %d, want %d", got, cutoff)
	}
	if !deleteRequested.Load() {
		t.Error("edge record was not deleted after resumed commit")
	}
}
