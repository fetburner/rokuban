// Package objstore defines the storage contract for derived media objects.
package objstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// DefaultURLExpiry keeps one signed media URL valid for a full day, within SigV4's seven-day limit.
const DefaultURLExpiry = 24 * time.Hour

var (
	// ErrNotFound means the requested key does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrAlreadyExists means a write would reuse an existing key.
	ErrAlreadyExists = errors.New("object key already exists")
	// ErrChecksumMismatch means the uploaded bytes did not match the expected SHA-256.
	ErrChecksumMismatch = errors.New("object checksum mismatch")
)

// Object describes a completed object returned by Head or List.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	SHA256       string
}

// Store is the durable object contract used for derived media.
//
// Put must publish all bytes or none, must reject a reused key, and must return only
// after the object is durable. List may lag, but it must never return an incomplete
// multipart upload.
type Store interface {
	Put(ctx context.Context, key string, src io.Reader, size int64, sha256 [32]byte, contentType string) error
	Head(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]Object, error)
}

// Local is implemented by stores that can serve objects from a seekable reader.
type Local interface {
	Open(ctx context.Context, key string) (io.ReadSeekCloser, Object, error)
}

// SignedURL is implemented by stores that can issue browser-readable URLs.
type SignedURL interface {
	Presign(ctx context.Context, key, method string, expires time.Duration, contentType, filename string) (string, error)
}
