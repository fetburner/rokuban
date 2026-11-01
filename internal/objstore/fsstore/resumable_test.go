package fsstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/objstore"
)

var errSimulatedStop = errors.New("simulated process stop")

// failAfterReader は limit バイトを読んだ後に errSimulatedStop を返す。プロセスが途中で落ちた模擬。
type failAfterReader struct {
	reader *bytes.Reader
	limit  int64
	read   int64
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, errSimulatedStop
	}
	if remaining := r.limit - r.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func fsTestBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((i*13 + i/241) % 249)
	}
	return out
}

// TestResumeDropsBytesPastCheckpoint は、checkpoint より後ろに落ちる前の未確定のバイトが残っていても、
// 再開で捨てられ、最終的な内容と長さが元どおりになることを確かめる（Truncate の検査）。
func TestResumeDropsBytesPastCheckpoint(t *testing.T) {
	root := t.TempDir()
	objects, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "encoded/torn.mp4"
	data := fsTestBytes(11 << 20)
	state, err := objects.StartUpload(ctx, key, int64(len(data)), sha256.Sum256(data), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	state, offset, err := objects.ContinueUpload(ctx, state, &failAfterReader{reader: bytes.NewReader(data), limit: 7 << 20})
	if !errors.Is(err, errSimulatedStop) || offset != 7<<20 {
		t.Fatalf("first ContinueUpload = (offset %d, %v), want offset %d and the simulated stop", offset, err, 7<<20)
	}

	var st uploadState
	if err := json.Unmarshal(state, &st); err != nil {
		t.Fatal(err)
	}
	junk := bytes.Repeat([]byte{0xEE}, 6<<20)
	staging, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(st.Staging)), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staging.Write(junk); err != nil {
		t.Fatal(err)
	}
	if err := staging.Close(); err != nil {
		t.Fatal(err)
	}

	state, offset, err = objects.ContinueUpload(ctx, state, bytes.NewReader(data[offset:]))
	if err != nil || offset != int64(len(data)) {
		t.Fatalf("second ContinueUpload = (offset %d, %v), want offset %d", offset, err, len(data))
	}
	if err := objects.CompleteUpload(ctx, state); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, key))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("published object has %d bytes that differ from the %d source bytes", len(got), len(data))
	}
}

// TestCompleteExistingKeyAcceptsSameContentOnly は、同じ key に同じ内容があれば Complete は成功し、
// 違う内容なら ErrAlreadyExists を返して既存の内容を変えないことを確かめる。
func TestCompleteExistingKeyAcceptsSameContentOnly(t *testing.T) {
	objects, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "encoded/existing.mp4"
	data := []byte("first content")
	if err := objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	same, err := objects.StartUpload(ctx, key, int64(len(data)), sha256.Sum256(data), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload same: %v", err)
	}
	same, _, err = objects.ContinueUpload(ctx, same, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContinueUpload same: %v", err)
	}
	if err := objects.CompleteUpload(ctx, same); err != nil {
		t.Fatalf("CompleteUpload with the same content = %v, want nil", err)
	}

	other := []byte("other content")
	diff, err := objects.StartUpload(ctx, key, int64(len(other)), sha256.Sum256(other), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload different: %v", err)
	}
	diff, _, err = objects.ContinueUpload(ctx, diff, bytes.NewReader(other))
	if err != nil {
		t.Fatalf("ContinueUpload different: %v", err)
	}
	if err := objects.CompleteUpload(ctx, diff); !errors.Is(err, objstore.ErrAlreadyExists) {
		t.Fatalf("CompleteUpload with different content = %v, want ErrAlreadyExists", err)
	}
	body, _, err := objects.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("existing object changed to %q", got)
	}
}

// TestAbortNeverRemovesPublishedObject は、Abort が最終パスを消さないことを確かめる。
// 偽造した state が最終パスを指していても、接頭辞の無い名前は拒否されることも含む。
func TestAbortNeverRemovesPublishedObject(t *testing.T) {
	objects, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "encoded/published.mp4"
	data := []byte("published bytes")
	if err := objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	forged, err := json.Marshal(uploadState{Key: key, Size: int64(len(data)), Staging: key, Hash: []byte{}})
	if err != nil {
		t.Fatal(err)
	}
	if gone, err := objects.AbortUpload(ctx, forged); !errors.Is(err, objstore.ErrUploadInvalid) || gone {
		t.Fatalf("AbortUpload with a final-path state = (%v, %v), want ErrUploadInvalid and gone=false", gone, err)
	}
	if _, err := objects.Head(ctx, key); err != nil {
		t.Fatalf("Head after abort with a forged state: %v", err)
	}
}

func TestContinueWithMissingStagingIsUploadInvalid(t *testing.T) {
	root := t.TempDir()
	objects, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := []byte("staging will vanish")
	state, err := objects.StartUpload(ctx, "encoded/vanish.mp4", int64(len(data)), sha256.Sum256(data), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	var st uploadState
	if err := json.Unmarshal(state, &st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(st.Staging))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := objects.ContinueUpload(ctx, state, bytes.NewReader(data)); !errors.Is(err, objstore.ErrUploadInvalid) {
		t.Fatalf("ContinueUpload without staging = %v, want ErrUploadInvalid", err)
	}
}
