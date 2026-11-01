package fsink

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestSaveCheckpointRestoresHashState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp")
	data := []byte("persisted prefix")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	hasher := sha256.New()
	_, _ = hasher.Write(data)
	var saved Checkpoint
	if err := SaveCheckpoint(file, int64(len(data)), hasher, func(checkpoint Checkpoint) error {
		saved = checkpoint
		return nil
	}); err != nil {
		t.Fatalf("SaveCheckpoint() error = %v", err)
	}
	if saved.Offset != int64(len(data)) {
		t.Fatalf("saved offset = %d, want %d", saved.Offset, len(data))
	}
	resumed := sha256.New()
	if err := RestoreHash(resumed, saved.HashState); err != nil {
		t.Fatalf("RestoreHash() error = %v", err)
	}
	if _, err := resumed.Write([]byte(" tail")); err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(resumed.Sum(nil)), digest([]byte("persisted prefix tail")); got != want {
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

func TestRemoveTempRemovesOnlyTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rokuban-ingest-remove")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemp(path); err != nil {
		t.Fatalf("RemoveTemp() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(%q) error = %v, want os.ErrNotExist", path, err)
	}
}

type partialErrorWriter struct {
	n   int
	err error
}

func (w partialErrorWriter) Write(p []byte) (int, error) { return w.n, w.err }

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ io.Writer = (*HashingWriter)(nil)
