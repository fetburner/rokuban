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
		checkPutPublishesCompleteBytes(t, newStore)
	})
	t.Run("missing key and delete semantics", func(t *testing.T) {
		checkMissingKeyAndDeleteSemantics(t, newStore)
	})
	t.Run("checksum mismatch leaves no visible object", func(t *testing.T) {
		checkChecksumMismatchLeavesNoVisibleObject(t, newStore)
	})
	t.Run("list never exposes an incomplete write", func(t *testing.T) {
		checkListHidesIncompleteWrite(t, newStore)
	})

	// 再開できるアップロードと範囲読みは、それを満たす実装だけに走らせる。
	if _, ok := newStore(t).(objstore.Resumable); ok {
		t.Run("resumable upload stopped and continued yields the same bytes", func(t *testing.T) {
			checkResumableStopAndContinue(t, newStore(t))
		})
		t.Run("resumable upload with a wrong digest publishes nothing", func(t *testing.T) {
			checkResumableWrongDigestPublishesNothing(t, newStore(t))
		})
		t.Run("resumable upload abort repeats until gone", func(t *testing.T) {
			checkResumableAbortRepeatsUntilGone(t, newStore(t))
		})
		t.Run("resumable upload rejects short and long sources", func(t *testing.T) {
			checkResumableSourceLength(t, newStore(t))
		})
		t.Run("range reader starts at offset", func(t *testing.T) {
			checkRangeReaderStartsAtOffset(t, newStore(t))
		})
	}
}

func checkPutPublishesCompleteBytes(t *testing.T, newStore func(*testing.T) objstore.Store) {
	t.Helper()
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
}

func checkMissingKeyAndDeleteSemantics(t *testing.T, newStore func(*testing.T) objstore.Store) {
	t.Helper()
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
}

func checkChecksumMismatchLeavesNoVisibleObject(t *testing.T, newStore func(*testing.T) objstore.Store) {
	t.Helper()
	objects := newStore(t)
	key := testKey(t)
	payload := []byte("untrusted payload")
	if err := objects.Put(t.Context(), key, bytes.NewReader(payload), int64(len(payload)), [sha256.Size]byte{1}, "application/octet-stream"); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("Put with wrong checksum error = %v, want ErrChecksumMismatch", err)
	}
	if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Head after rejected Put error = %v, want ErrNotFound", err)
	}
}

func checkListHidesIncompleteWrite(t *testing.T, newStore func(*testing.T) objstore.Store) {
	t.Helper()
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

// checkResumableStopAndContinue は、途中で ctx を切って返った state と offset だけから続きを送っても、
// 同じバイト列のオブジェクトになることを確かめる。newStore は呼ぶたびに別の置き場を返すので、
// 1 回目と 2 回目で同じ Store を使う。別プロセスからの再開は各実装の単体テストで Store を作り直して確かめる。
func checkResumableStopAndContinue(t *testing.T, objects objstore.Store) {
	t.Helper()
	resumable := objects.(objstore.Resumable)
	key := testKey(t)
	data := deterministicBytes(11 << 20)
	digest := sha256.Sum256(data)
	state, err := resumable.StartUpload(t.Context(), key, int64(len(data)), digest, "application/octet-stream")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := &stopAfterReader{reader: bytes.NewReader(data), limit: 7 << 20, cancel: cancel}
	state, offset, err := resumable.ContinueUpload(ctx, state, stopped)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first ContinueUpload error = %v, want context.Canceled", err)
	}
	if offset <= 0 || offset >= int64(len(data)) {
		t.Fatalf("first ContinueUpload offset = %d, want a partial offset in (0, %d)", offset, len(data))
	}

	state, offset, err = resumable.ContinueUpload(t.Context(), state, bytes.NewReader(data[offset:]))
	if err != nil {
		t.Fatalf("second ContinueUpload: %v", err)
	}
	if offset != int64(len(data)) {
		t.Fatalf("second ContinueUpload offset = %d, want %d", offset, len(data))
	}
	if err := resumable.CompleteUpload(t.Context(), state); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	got := readObject(t, objects, key)
	if sha256.Sum256(got) != digest || len(got) != len(data) {
		t.Fatalf("resumed object has %d bytes, want the original %d bytes", len(got), len(data))
	}
}

// checkResumableWrongDigestPublishesNothing は、送りながら計算した SHA-256 が違うとき、
// Complete が何も公開しないことを確かめる。
func checkResumableWrongDigestPublishesNothing(t *testing.T, objects objstore.Store) {
	t.Helper()
	resumable := objects.(objstore.Resumable)
	key := testKey(t)
	data := []byte("resumable payload")
	state, err := resumable.StartUpload(t.Context(), key, int64(len(data)), sha256.Sum256([]byte("other")), "application/octet-stream")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	state, _, err = resumable.ContinueUpload(t.Context(), state, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContinueUpload: %v", err)
	}
	if err := resumable.CompleteUpload(t.Context(), state); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("CompleteUpload with a wrong digest error = %v, want ErrChecksumMismatch", err)
	}
	if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Head after rejected Complete error = %v, want ErrNotFound", err)
	}
	abortUntilGone(t, resumable, state)
}

// checkResumableAbortRepeatsUntilGone は、Abort 後の Complete が公開せずに失敗し、
// Abort を繰り返すといずれ gone が true になることを確かめる。
func checkResumableAbortRepeatsUntilGone(t *testing.T, objects objstore.Store) {
	t.Helper()
	resumable := objects.(objstore.Resumable)
	key := testKey(t)
	data := deterministicBytes(2 << 20)
	digest := sha256.Sum256(data)
	state, err := resumable.StartUpload(t.Context(), key, int64(len(data)), digest, "application/octet-stream")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	state, _, err = resumable.ContinueUpload(t.Context(), state, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContinueUpload: %v", err)
	}
	abortUntilGone(t, resumable, state)
	if err := resumable.CompleteUpload(t.Context(), state); !errors.Is(err, objstore.ErrUploadInvalid) {
		t.Fatalf("CompleteUpload after abort error = %v, want ErrUploadInvalid", err)
	}
	if _, err := objects.Head(t.Context(), key); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Head after aborted upload error = %v, want ErrNotFound", err)
	}
}

// checkResumableSourceLength は、source が size より短いときと長いときの両方を ErrChecksumMismatch で拒むことを確かめる。
func checkResumableSourceLength(t *testing.T, objects objstore.Store) {
	t.Helper()
	resumable := objects.(objstore.Resumable)
	key := testKey(t)
	data := []byte("exact length payload")
	state, err := resumable.StartUpload(t.Context(), key, int64(len(data)), sha256.Sum256(data), "application/octet-stream")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	// 短い場合と長い場合は同じ staging（同じ UploadId）を指すので、最後に 1 回だけ Abort する。
	_, _, err = resumable.ContinueUpload(t.Context(), state, bytes.NewReader(data[:10]))
	if !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("ContinueUpload with a short source error = %v, want ErrChecksumMismatch", err)
	}
	long, _, err := resumable.ContinueUpload(t.Context(), state, bytes.NewReader(append(bytes.Clone(data), 'x')))
	if !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("ContinueUpload with a long source error = %v, want ErrChecksumMismatch", err)
	}
	abortUntilGone(t, resumable, long)
}

// checkRangeReaderStartsAtOffset は、GetFrom(offset) が data[offset:] を返すことを確かめる。
func checkRangeReaderStartsAtOffset(t *testing.T, objects objstore.Store) {
	t.Helper()
	ranged, ok := objects.(objstore.RangeReader)
	if !ok {
		t.Fatal("store implements objstore.Resumable but not objstore.RangeReader")
	}
	key := testKey(t)
	data := deterministicBytes(64 << 10)
	if err := objects.Put(t.Context(), key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "application/octet-stream"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, offset := range []int64{0, 12345, int64(len(data)) - 1} {
		body, err := ranged.GetFrom(t.Context(), key, offset)
		if err != nil {
			t.Fatalf("GetFrom(%d): %v", offset, err)
		}
		got, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("reading GetFrom(%d): read %v, close %v", offset, readErr, closeErr)
		}
		if !bytes.Equal(got, data[offset:]) {
			t.Fatalf("GetFrom(%d) returned %d bytes, want data[%d:] with %d bytes", offset, len(got), offset, len(data)-int(offset))
		}
	}
}

// abortUntilGone は AbortUpload を gone が true になるまで繰り返す。送信中のパートは Abort の後に残りうる。
func abortUntilGone(t *testing.T, resumable objstore.Resumable, state []byte) {
	t.Helper()
	for range 10 {
		gone, err := resumable.AbortUpload(t.Context(), state)
		if err != nil {
			t.Fatalf("AbortUpload: %v", err)
		}
		if gone {
			return
		}
	}
	t.Fatal("AbortUpload never reported the upload gone")
}

// stopAfterReader は limit バイトを読んだ後に ctx を切る。以降の Read は context.Canceled を返す。
type stopAfterReader struct {
	reader *bytes.Reader
	limit  int64
	read   int64
	cancel context.CancelFunc
}

func (r *stopAfterReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		r.cancel()
		return 0, context.Canceled
	}
	if remaining := r.limit - r.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func deterministicBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((i*31 + i/251) % 251)
	}
	return out
}
