// Package contracttest contains shared behavioral checks for objstore.Store implementations.
package contracttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/objstore"
)

// Run checks the behavioral contract shared by every object store implementation.
// newStore must return an isolated store for each subtest.
func Run(t *testing.T, newStore func(*testing.T) objstore.Store) {
	t.Helper()

	t.Run("put publishes complete durable bytes and never overwrites", func(t *testing.T) {
		objects := newStore(t)
		key := testKey(t)
		want := []byte("complete object payload")
		if err := objects.Put(t.Context(), key, bytes.NewReader(want), int64(len(want)), sha256.Sum256(want), "application/octet-stream"); err != nil {
			t.Fatalf("Put: %v", err)
		}

		gotObject, err := objects.Head(t.Context(), key)
		if err != nil {
			t.Fatalf("Head after Put: %v", err)
		}
		if gotObject.Key != key || gotObject.Size != int64(len(want)) {
			t.Fatalf("Head after Put = %+v, want key %q and size %d", gotObject, key, len(want))
		}
		got := readObject(t, objects, key)
		if !bytes.Equal(got, want) {
			t.Fatalf("read after Put = %q, want %q", got, want)
		}

		other := []byte("replacement payload")
		err = objects.Put(t.Context(), key, bytes.NewReader(other), int64(len(other)), sha256.Sum256(other), "application/octet-stream")
		if err == nil {
			t.Fatal("Put with reused key succeeded, want a key conflict")
		}
		if !errors.Is(err, objstore.ErrAlreadyExists) && !errors.Is(err, objstore.ErrChecksumMismatch) {
			t.Fatalf("Put with reused key error = %v, want a key conflict", err)
		}
		if got := readObject(t, objects, key); !bytes.Equal(got, want) {
			t.Fatalf("reused key now contains %q, want original bytes %q", got, want)
		}
	})

	t.Run("missing key and delete semantics", func(t *testing.T) {
		objects := newStore(t)
		key := testKey(t)
		if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("Head for missing key error = %v, want ErrNotFound", err)
		}
		if err := objects.Delete(t.Context(), key); err != nil {
			t.Fatalf("Delete missing key: %v", err)
		}
		payload := []byte("delete me")
		if err := objects.Put(t.Context(), key, bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "application/octet-stream"); err != nil {
			t.Fatalf("Put before Delete: %v", err)
		}
		if err := objects.Delete(t.Context(), key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := objects.Delete(t.Context(), key); err != nil {
			t.Fatalf("repeated Delete: %v", err)
		}
		if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("Head after Delete error = %v, want ErrNotFound", err)
		}
	})

	t.Run("checksum mismatch leaves no visible object", func(t *testing.T) {
		objects := newStore(t)
		key := testKey(t)
		payload := []byte("untrusted payload")
		if err := objects.Put(t.Context(), key, bytes.NewReader(payload), int64(len(payload)), [sha256.Size]byte{1}, "application/octet-stream"); !errors.Is(err, objstore.ErrChecksumMismatch) {
			t.Fatalf("Put with wrong checksum error = %v, want ErrChecksumMismatch", err)
		}
		if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("Head after rejected Put error = %v, want ErrNotFound", err)
		}
	})

	t.Run("list never exposes an incomplete write", func(t *testing.T) {
		objects := newStore(t)
		key := testKey(t)
		prefix := key[:strings.LastIndexByte(key, '/')+1]
		payload := []byte("blocked complete payload")
		reader := &blockedReader{started: make(chan struct{}), release: make(chan struct{}), data: payload}
		putDone := make(chan error, 1)
		putFinished := false
		go func() {
			putDone <- objects.Put(context.Background(), key, reader, int64(len(payload)), sha256.Sum256(payload), "application/octet-stream")
		}()
		defer func() {
			select {
			case <-reader.release:
			default:
				close(reader.release)
			}
			if !putFinished {
				select {
				case err := <-putDone:
					if err != nil {
						t.Errorf("Put after releasing blocked reader: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("Put did not stop after releasing blocked reader")
				}
			}
		}()

		select {
		case <-reader.started:
		case <-time.After(5 * time.Second):
			t.Fatal("Put did not reach the blocked source read")
		}
		listed, err := objects.List(t.Context(), prefix)
		if err != nil {
			t.Fatalf("List while Put is blocked: %v", err)
		}
		if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("Head while Put is blocked = %v, want ErrNotFound", err)
		}
		for _, object := range listed {
			if object.Key == key {
				t.Fatalf("List exposed incomplete key %q", key)
			}
		}

		close(reader.release)
		select {
		case err := <-putDone:
			putFinished = true
			if err != nil {
				t.Fatalf("Put after releasing blocked reader: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Put did not finish after releasing blocked reader")
		}
		listed, err = objects.List(t.Context(), prefix)
		if err != nil {
			t.Fatalf("List after Put: %v", err)
		}
		for _, object := range listed {
			if object.Key == key && object.Size != int64(len(payload)) {
				t.Fatalf("List returned incomplete metadata for %q: %+v", key, object)
			}
		}
	})
}

func readObject(t *testing.T, objects objstore.Store, key string) []byte {
	t.Helper()
	if local, ok := objects.(objstore.Local); ok {
		body, _, err := local.Open(t.Context(), key)
		if err != nil {
			t.Fatalf("Open %q: %v", key, err)
		}
		defer func() { _ = body.Close() }()
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("reading %q: %v", key, err)
		}
		return data
	}
	reader, ok := objects.(objstore.Reader)
	if !ok {
		t.Fatal("store implements neither objstore.Local nor objstore.Reader")
	}
	body, err := reader.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	return data
}

func testKey(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("contracttest/%d/%s.bin", time.Now().UnixNano(), name)
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
