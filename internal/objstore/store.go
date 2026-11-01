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
	// ErrCredentials means the request failed because the credentials could not be retrieved,
	// have expired, or were not recognized. It is joined to the underlying error.
	ErrCredentials = errors.New("object store credentials are unusable")
	// ErrReadOnly means the object store was initialized without write capabilities.
	ErrReadOnly = errors.New("object store is read-only")
	// ErrUploadInvalid は、保存した再開状態ではもう続きを送れない・完了できないことを表す
	// （S3 の NoSuchUpload / InvalidPart / InvalidPartOrder、fsstore の staging が無い・短い等）。
	// 呼び出し側は状態を捨てて新しいアップロードを最初から始める。
	ErrUploadInvalid = errors.New("resumable upload is no longer valid")
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

// Reader is implemented by stores that can stream a completed object's contents.
type Reader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// Local is implemented by stores that can serve objects from a seekable reader.
type Local interface {
	Open(ctx context.Context, key string) (io.ReadSeekCloser, Object, error)
}

// Resumable は、1 つのアップロードをプロセスをまたいで続けられる置き場が実装する。
// state は実装が marshal する不透明なバイト列で、呼び出し側は DB に保存してそのまま返す。
type Resumable interface {
	// StartUpload は key に size バイト・SHA-256 が sha256 のオブジェクトを送るアップロードを始める。バイトはまだ送らない。
	StartUpload(ctx context.Context, key string, size int64, sha256 [32]byte, contentType string) (state []byte, err error)
	// ContinueUpload は state が記録する offset から始まる src を読み、size に達するか ctx が終わるまで送る。
	// 返す newState と offset は、置き場に確実に残ったバイトだけを表す。err が非 nil でも呼び出し側は両方を保存してよい。
	ContinueUpload(ctx context.Context, state []byte, src io.Reader) (newState []byte, offset int64, err error)
	// CompleteUpload は、size バイトすべてを送り終え、送りながら計算した SHA-256 が一致したときだけ公開する。
	CompleteUpload(ctx context.Context, state []byte) error
	// AbortUpload はアップロードを破棄する。gone は「アップロードの残骸が何も無い」と確認できたときだけ true。
	AbortUpload(ctx context.Context, state []byte) (gone bool, err error)
}

// RangeReader は、完了したオブジェクトを offset から読める置き場が実装する。
type RangeReader interface {
	GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
}

// SignedURL is implemented by stores that can issue browser-readable URLs.
type SignedURL interface {
	Presign(ctx context.Context, key, method string, expires time.Duration, contentType, filename string) (string, error)
}
