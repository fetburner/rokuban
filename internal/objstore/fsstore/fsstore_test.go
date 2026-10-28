package fsstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

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
