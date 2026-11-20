package fsink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHashTransferFileAndVerifyExistingFileSHA256(t *testing.T) {
	t.Parallel()
	const content = "transfer content"
	path := filepath.Join(t.TempDir(), "original.ts")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing transfer file: %v", err)
	}
	wantDigest := sha256.Sum256([]byte(content))

	digest, size, err := HashTransferFile(context.Background(), path)
	if err != nil {
		t.Fatalf("HashTransferFile: %v", err)
	}
	if !bytes.Equal(digest, wantDigest[:]) || size != int64(len(content)) {
		t.Fatalf("HashTransferFile = (%x, %d), want (%x, %d)", digest, size, wantDigest, len(content))
	}

	digest, size, matches, err := VerifyExistingFileSHA256(context.Background(), path, int64(len(content)), wantDigest[:])
	if err != nil || !matches || !bytes.Equal(digest, wantDigest[:]) || size != int64(len(content)) {
		t.Fatalf("matching VerifyExistingFileSHA256 = (%x, %d, %t, %v), want (%x, %d, true, nil)",
			digest, size, matches, err, wantDigest, len(content))
	}

	wrongDigest := sha256.Sum256([]byte("different content"))
	_, size, matches, err = VerifyExistingFileSHA256(context.Background(), path, int64(len(content)), wrongDigest[:])
	if err != nil || matches || size != int64(len(content)) {
		t.Fatalf("different-digest VerifyExistingFileSHA256 = (%d, %t, %v), want (%d, false, nil)", size, matches, err, len(content))
	}

	_, size, matches, err = VerifyExistingFileSHA256(context.Background(), path, int64(len(content)+1), wantDigest[:])
	if err != nil || matches || size != int64(len(content)) {
		t.Fatalf("different-size VerifyExistingFileSHA256 = (%d, %t, %v), want (%d, false, nil)", size, matches, err, len(content))
	}

	_, size, matches, err = VerifyExistingFileSHA256(context.Background(), filepath.Join(t.TempDir(), "missing"), int64(len(content)), wantDigest[:])
	if err != nil || matches || size != 0 {
		t.Fatalf("missing VerifyExistingFileSHA256 = (%d, %t, %v), want (0, false, nil)", size, matches, err)
	}
}

func TestContextReaderReturnsCancellationBeforeReading(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var dst [1]byte
	n, err := NewContextReader(ctx, bytes.NewReader([]byte("x"))).Read(dst[:])
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = (%d, %v), want (0, context.Canceled)", n, err)
	}
}
