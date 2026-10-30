package fsstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/contracttest"
)

func TestStoreContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T) objstore.Store {
		objects, err := New(t.TempDir())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return objects
	})
}

func TestPresignIsNotSupported(t *testing.T) {
	objects, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Presign(context.Background(), "encoded/a.mp4", "GET", 0, "", ""); err == nil {
		t.Fatal("Presign succeeded for a filesystem store")
	}
}

func TestStoreRejectsReservedStagingKey(t *testing.T) {
	root := t.TempDir()
	objects, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	key := "encoded/.rokuban-media-asset-user-file"
	data := []byte("canonical object")
	if err := objects.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err == nil {
		t.Fatalf("Put accepted key %q from the reserved staging namespace", key)
	}
	if _, err := os.Stat(filepath.Join(root, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved object stat error = %v, want os.ErrNotExist", err)
	}
}
