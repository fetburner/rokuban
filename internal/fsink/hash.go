package fsink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// HashTransferFile はファイルの SHA-256 とサイズを返す。
// TestHashTransferFileAndVerifyExistingFileSHA256 は digest とサイズを固定する。
func HashTransferFile(ctx context.Context, path string) ([]byte, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("stating transfer file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("transfer path is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening transfer file: %w", err)
	}
	hasher := sha256.New()
	count, err := io.Copy(hasher, NewContextReader(ctx, f))
	closeErr := f.Close()
	if err != nil {
		return nil, count, fmt.Errorf("hashing transfer file: %w", err)
	}
	if closeErr != nil {
		return nil, count, fmt.Errorf("closing transfer file after hashing: %w", closeErr)
	}
	if count != info.Size() {
		return nil, count, fmt.Errorf("transfer file changed while hashing: read=%d size=%d", count, info.Size())
	}
	return hasher.Sum(nil), count, nil
}

// VerifyExistingFileSHA256 は既存の canonical を、期待サイズと SHA-256 の両方で照合する。
// ingest と storage transfer が EEXIST を採用する条件を同じ実装に保つ。
// TestHashTransferFileAndVerifyExistingFileSHA256 は一致・不一致・欠落を固定する。
func VerifyExistingFileSHA256(ctx context.Context, path string, expectedSize int64, expectedSHA256 []byte) ([]byte, int64, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("stating existing destination: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, info.Size(), false, fmt.Errorf("existing destination is not a regular file")
	}
	if info.Size() != expectedSize {
		return nil, info.Size(), false, nil
	}
	digest, size, err := HashTransferFile(ctx, path)
	if err != nil {
		return nil, size, false, err
	}
	return digest, size, size == expectedSize && bytes.Equal(digest, expectedSHA256), nil
}
