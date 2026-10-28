package fsstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/objstore"
)

func TestStorePublishesCompleteObjectWithoutOverwrite(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("complete object")
	digest := sha256.Sum256(data)
	if err := store.Put(ctx, "encoded/1.mp4", bytes.NewReader(data), int64(len(data)), digest, "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(ctx, "encoded/1.mp4", bytes.NewReader(data), int64(len(data)), digest, "video/mp4"); !errors.Is(err, objstore.ErrAlreadyExists) {
		t.Fatalf("second Put error = %v, want ErrAlreadyExists", err)
	}

	file, object, err := store.Open(ctx, "encoded/1.mp4")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("closing opened object: %v", err)
		}
	}()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("reading opened object: %v", err)
	}
	if !bytes.Equal(got, data) || object.Size != int64(len(data)) {
		t.Fatalf("opened object = %q, size %d; want %q, size %d", got, object.Size, data, len(data))
	}
}

func TestStoreRejectsChecksumMismatchBeforePublication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("untrusted bytes")
	if err := store.Put(ctx, "encoded/2.mp4", bytes.NewReader(data), int64(len(data)), [32]byte{1}, "video/mp4"); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("Put error = %v, want ErrChecksumMismatch", err)
	}
	if _, err := os.Stat(filepath.Join(root, "encoded/2.mp4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched object stat error = %v, want os.ErrNotExist", err)
	}
}

func TestStoreRejectsReservedStagingKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	key := "encoded/.rokuban-media-asset-user-file"
	data := []byte("canonical object")
	if err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err == nil {
		t.Fatalf("Put accepted key %q from the reserved staging namespace", key)
	}
	if _, err := os.Stat(filepath.Join(root, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved object stat error = %v, want os.ErrNotExist", err)
	}
}

func TestStoreListAndDeleteAreIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("object")
	if err := store.Put(ctx, "encoded/a.mp4", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	objects, err := store.List(ctx, "encoded/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "encoded/a.mp4" {
		t.Fatalf("List = %+v, want one encoded/a.mp4 object", objects)
	}
	if err := store.Delete(ctx, "encoded/a.mp4"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := store.Delete(ctx, "encoded/a.mp4"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := store.Head(ctx, "encoded/a.mp4"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Head after Delete error = %v, want ErrNotFound", err)
	}
}

func TestListDoesNotExposeInProgressPut(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("completed object")
	reader := &blockedReader{started: make(chan struct{}), release: make(chan struct{}), data: data}
	var releaseOnce sync.Once
	releaseReader := func() { releaseOnce.Do(func() { close(reader.release) }) }
	putDone := make(chan error, 1)
	go func() {
		putDone <- store.Put(ctx, "encoded/in-progress.mp4", reader, int64(len(data)), sha256.Sum256(data), "video/mp4")
	}()
	defer releaseReader()

	select {
	case <-reader.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Put did not reach its blocked source read")
	}

	objects, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("List while Put is blocked: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("List while Put is blocked = %+v, want no visible incomplete object", objects)
	}

	releaseReader()
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatalf("Put after releasing source: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Put did not finish after releasing its source")
	}

	objects, err = store.List(ctx, "")
	if err != nil {
		t.Fatalf("List after Put completes: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "encoded/in-progress.mp4" || objects[0].Size != int64(len(data)) {
		t.Fatalf("List after Put = %+v, want the complete encoded/in-progress.mp4 object", objects)
	}
}

type blockedReader struct {
	started chan struct{}
	release chan struct{}
	data    []byte
	done    bool
}

func (r *blockedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	close(r.started)
	<-r.release
	r.done = true
	return copy(p, r.data), nil
}
