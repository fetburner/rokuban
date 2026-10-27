package fsink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHashingWriterHashesAcceptedBytes(t *testing.T) {
	input := []byte("accepted prefix plus rejected suffix")
	injectedErr := errors.New("injected partial write")
	hasher := sha256.New()
	writer := NewHashingWriter(partialErrorWriter{n: len("accepted prefix"), err: injectedErr}, hasher)

	n, err := writer.Write(input)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("Write() error = %v, want injected partial write", err)
	}
	if n != len("accepted prefix") {
		t.Fatalf("Write() bytes = %d, want %d", n, len("accepted prefix"))
	}
	if got, want := hex.EncodeToString(hasher.Sum(nil)), digest(input[:n]); got != want {
		t.Errorf("hash of accepted bytes = %s, want %s", got, want)
	}
}

func TestReplayTempFileReadsExistingBytesIntoHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-replay")
	want := []byte("bytes already written to the temp file")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	hasher := sha256.New()
	offset, err := ReplayTempFile(context.Background(), path, hasher)
	if err != nil {
		t.Fatalf("ReplayTempFile() error = %v", err)
	}
	if offset != int64(len(want)) {
		t.Fatalf("replayed offset = %d, want %d", offset, len(want))
	}
	if got, wantHash := hex.EncodeToString(hasher.Sum(nil)), digest(want); got != wantHash {
		t.Fatalf("replayed SHA-256 = %s, want %s", got, wantHash)
	}
}

func TestReplayTempFileWithCheckpointResumesHashFromSavedOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-test")
	prefix := []byte("persisted prefix")
	suffix := []byte(" and new tail")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(prefix); err != nil {
		t.Fatal(err)
	}
	prefixHash := sha256.New()
	_, _ = prefixHash.Write(prefix)
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpoint(path, int64(len(prefix)), prefixHash, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(suffix); err != nil {
		t.Fatal(err)
	}

	hasher := sha256.New()
	offset, complete, err := ReplayTempFileWithCheckpoint(context.Background(), path, file, hasher, nil)
	if err != nil || !complete {
		t.Fatalf("replay = (complete=%v, err=%v), want complete", complete, err)
	}
	if want := int64(len(prefix) + len(suffix)); offset != want {
		t.Fatalf("replayed offset = %d, want %d", offset, want)
	}
	if got, want := hex.EncodeToString(hasher.Sum(nil)), digest(append(bytes.Clone(prefix), suffix...)); got != want {
		t.Fatalf("resumed SHA-256 = %s, want %s", got, want)
	}
}

func TestReplayTempFileWithCheckpointInterruptedReplaySavesAndUsesOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-cancel")
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &cancelAfterFirstWriteHash{Hash: sha256.New(), cancel: cancel}
	offset, complete, err := ReplayTempFileWithCheckpoint(ctx, path, file, first, nil)
	if complete || !errors.Is(err, context.Canceled) {
		t.Fatalf("replay = (complete=%v, err=%v), want incomplete context.Canceled", complete, err)
	}
	if first.written == 0 || first.written >= int64(len(data)) || offset != first.written {
		t.Fatalf("offset = %d, hashed = %d, size = %d; want offset == hashed within (0, size)", offset, first.written, len(data))
	}

	resumed := sha256.New()
	saved, restored := RestoreCheckpoint(path, int64(len(data)), resumed)
	if !restored || saved != first.written {
		t.Fatalf("saved checkpoint = (%d, %v), want (%d, true)", saved, restored, first.written)
	}
	if _, complete, err := ReplayTempFileWithCheckpoint(context.Background(), path, file, resumed, nil); err != nil || !complete {
		t.Fatalf("resumed replay = (complete=%v, err=%v)", complete, err)
	}
	if got, want := hex.EncodeToString(resumed.Sum(nil)), digest(data); got != want {
		t.Fatalf("resumed SHA-256 = %s, want %s", got, want)
	}
}

func inodeOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestPublishLinkRefusesDifferentInodeAtDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "temp")
	dst := filepath.Join(dir, "canonical")
	// 同じ大きさで内容が違う。大きさだけを見る実装は見分けられない。
	if err := os.WriteFile(src, []byte("new content!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old content!"), 0o600); err != nil {
		t.Fatal(err)
	}
	dstBefore := inodeOf(t, dst)

	err := PublishLink(src, dst)
	if !errors.Is(err, os.ErrExist) || !errors.Is(err, ErrForeignDestination) {
		t.Fatalf("PublishLink() error = %v, want os.ErrExist and ErrForeignDestination", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "old content!" {
		t.Fatalf("canonical file = (%q, %v), want old content", got, err)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "new content!" {
		t.Fatalf("temp file = (%q, %v), want new content", got, err)
	}
	if !os.SameFile(dstBefore, inodeOf(t, dst)) || os.SameFile(inodeOf(t, src), inodeOf(t, dst)) {
		t.Fatal("canonical inode changed or became the temp inode")
	}
}

func TestPublishLinkTreatsSameInodeAtDestinationAsPublished(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "temp")
	dst := filepath.Join(dir, "canonical")
	if err := os.WriteFile(src, []byte("complete content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := PublishLink(src, dst); err != nil {
		t.Fatalf("PublishLink() with an already linked destination error = %v, want nil", err)
	}
}

func TestPublishLinkLinksWhenDestinationDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "temp")
	dst := filepath.Join(dir, "canonical")
	if err := os.WriteFile(src, []byte("complete content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishLink(src, dst); err != nil {
		t.Fatalf("PublishLink() error = %v", err)
	}
	if !os.SameFile(inodeOf(t, src), inodeOf(t, dst)) {
		t.Fatal("src and dst are not the same inode")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "complete content" {
		t.Fatalf("canonical file = (%q, %v), want complete content", got, err)
	}
}

func TestPublishLinkFailsWhenSourceIsMissing(t *testing.T) {
	dir := t.TempDir()
	err := PublishLink(filepath.Join(dir, "missing"), filepath.Join(dir, "canonical"))
	if err == nil || errors.Is(err, ErrForeignDestination) {
		t.Fatalf("PublishLink() error = %v, want a plain link failure", err)
	}
}

func TestRemoveTempRemovesTempAndCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-remove")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CheckpointPath(path), []byte("checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemp(path); err != nil {
		t.Fatalf("RemoveTemp() error = %v", err)
	}
	for _, candidate := range []string{path, CheckpointPath(path)} {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stat(%q) error = %v, want os.ErrNotExist", candidate, err)
		}
	}
}

func TestLockTempFileIsNonBlockingAndExistingLockDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp")
	first, created, err := LockTempFile(path)
	if err != nil {
		t.Fatalf("LockTempFile() error = %v", err)
	}
	if !created {
		t.Fatal("first lock reported an existing file, want newly created temp")
	}
	if _, err := LockExistingTempFile(path); !TempLockBusy(err) {
		t.Fatalf("second lock error = %v, want non-blocking flock conflict", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("releasing first lock: %v", err)
	}

	second, err := LockExistingTempFile(path)
	if err != nil {
		t.Fatalf("LockExistingTempFile() after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("releasing second lock: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := LockExistingTempFile(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LockExistingTempFile(missing) error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing temp stat error = %v, want os.ErrNotExist", err)
	}
}

type partialErrorWriter struct {
	n   int
	err error
}

func (w partialErrorWriter) Write(p []byte) (int, error) { return w.n, w.err }

type cancelAfterFirstWriteHash struct {
	hash.Hash
	cancel  context.CancelFunc
	written int64
}

func (h *cancelAfterFirstWriteHash) Write(p []byte) (int, error) {
	n, err := h.Hash.Write(p)
	h.written += int64(n)
	h.cancel()
	return n, err
}

func (h *cancelAfterFirstWriteHash) MarshalBinary() ([]byte, error) {
	return h.Hash.(encoding.BinaryMarshaler).MarshalBinary()
}

func (h *cancelAfterFirstWriteHash) UnmarshalBinary(data []byte) error {
	return h.Hash.(encoding.BinaryUnmarshaler).UnmarshalBinary(data)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ io.Writer = (*HashingWriter)(nil)

func TestReadContextHonorsCancellationBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := strings.NewReader("abcdef")
	buf := make([]byte, 3)

	n, err := ReadContext(ctx, reader, buf)
	if err != nil || n != 3 || string(buf) != "abc" {
		t.Fatalf("first read = (%d, %v, %q), want (3, nil, %q)", n, err, buf, "abc")
	}
	cancel()
	if n, err := ReadContext(ctx, reader, buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = (%d, %v), want (0, context.Canceled)", n, err)
	}
}
