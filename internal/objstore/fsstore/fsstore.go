// Package fsstore implements objstore.Store on a strong POSIX filesystem.
package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
)

// Store writes objects under root using same-directory staging and no-replace publication.
type Store struct {
	root string
}

// New creates a filesystem object store rooted at root.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("filesystem object store root is empty")
	}
	return &Store{root: root}, nil
}

// Put publishes src under key after validating its exact size and SHA-256.
func (s *Store) Put(ctx context.Context, key string, src io.Reader, size int64, wantSHA [32]byte, _ string) error {
	if size < 0 {
		return fmt.Errorf("negative object size %d", size)
	}
	if mediapath.IsGeneratedAssetTempFile(key) {
		return fmt.Errorf("object key %q uses the reserved staging prefix", key)
	}
	finalPath, err := mediapath.Resolve(s.root, key)
	if err != nil {
		return fmt.Errorf("resolving object key %q: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("creating object directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(finalPath), mediapath.GeneratedAssetTempFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("creating object staging file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("setting object permissions: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(temp, h), &contextReader{ctx: ctx, reader: io.LimitReader(src, size+1)})
	if copyErr != nil {
		_ = temp.Close()
		return fmt.Errorf("writing object staging file: %w", copyErr)
	}
	if n != size {
		_ = temp.Close()
		return fmt.Errorf("object size = %d, want %d", n, size)
	}
	var gotSHA [32]byte
	copy(gotSHA[:], h.Sum(nil))
	if gotSHA != wantSHA {
		_ = temp.Close()
		return objstore.ErrChecksumMismatch
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("syncing object staging file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing object staging file: %w", err)
	}
	if err := fsink.PublishLink(tempPath, finalPath); err != nil {
		if errors.Is(err, fsink.ErrForeignDestination) || errors.Is(err, os.ErrExist) {
			return errors.Join(objstore.ErrAlreadyExists, err)
		}
		return fmt.Errorf("publishing object: %w", err)
	}
	if err := fsink.SyncParentDir(finalPath); err != nil {
		return fmt.Errorf("syncing object directory: %w", err)
	}
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing object staging link: %w", err)
	}
	return nil
}

// Head returns metadata for one completed object.
func (s *Store) Head(_ context.Context, key string) (objstore.Object, error) {
	path, err := mediapath.Resolve(s.root, key)
	if err != nil {
		return objstore.Object{}, fmt.Errorf("resolving object key %q: %w", key, err)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return objstore.Object{}, objstore.ErrNotFound
	}
	if err != nil {
		return objstore.Object{}, fmt.Errorf("stating object %q: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		return objstore.Object{}, fmt.Errorf("object %q is not a regular file", key)
	}
	return objstore.Object{Key: key, Size: info.Size(), LastModified: info.ModTime()}, nil
}

// Open returns a seekable reader for a completed object.
func (s *Store) Open(_ context.Context, key string) (io.ReadSeekCloser, objstore.Object, error) {
	path, err := mediapath.Resolve(s.root, key)
	if err != nil {
		return nil, objstore.Object{}, fmt.Errorf("resolving object key %q: %w", key, err)
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, objstore.Object{}, objstore.ErrNotFound
	}
	if err != nil {
		return nil, objstore.Object{}, fmt.Errorf("opening object %q: %w", key, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, objstore.Object{}, fmt.Errorf("stating opened object %q: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, objstore.Object{}, fmt.Errorf("object %q is not a regular file", key)
	}
	return f, objstore.Object{Key: key, Size: info.Size(), LastModified: info.ModTime()}, nil
}

// Get opens a completed object for sequential reads.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f, _, err := s.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Delete removes a key and succeeds when it is already absent.
func (s *Store) Delete(_ context.Context, key string) error {
	path, err := mediapath.Resolve(s.root, key)
	if err != nil {
		return fmt.Errorf("resolving object key %q: %w", key, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting object %q: %w", key, err)
	}
	if err := fsink.SyncParentDir(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("syncing object directory after delete: %w", err)
	}
	return nil
}

// List returns regular files under prefix in filesystem traversal order.
func (s *Store) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	root, err := filepath.Abs(s.root)
	if err != nil {
		return nil, fmt.Errorf("resolving object root: %w", err)
	}
	var objects []objstore.Object
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if mediapath.IsGeneratedAssetTempFile(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		objects = append(objects, objstore.Object{Key: key, Size: info.Size(), LastModified: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing filesystem objects: %w", err)
	}
	return objects, nil
}

// Presign is not supported by filesystem objects.
func (s *Store) Presign(context.Context, string, string, time.Duration, string, string) (string, error) {
	return "", errors.New("filesystem objects do not have signed URLs")
}

// contextReader stops copying promptly when its context is cancelled.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// uploadState は ContinueUpload・CompleteUpload・AbortUpload が DB 経由で受け渡す再開状態。
// Staging は root からの相対パスで、pod ごとにマウント先が違っても解決できるようにする。
type uploadState struct {
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Staging string `json:"staging"`
	Offset  int64  `json:"offset"`
	Hash    []byte `json:"hash"`
}

// StartUpload は key の staging ファイルを作り、offset 0 の再開状態を返す。バイトはまだ書かない。
func (s *Store) StartUpload(_ context.Context, key string, size int64, wantSHA [32]byte, _ string) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("negative object size %d", size)
	}
	if mediapath.IsGeneratedAssetTempFile(key) {
		return nil, fmt.Errorf("object key %q uses the reserved staging prefix", key)
	}
	finalPath, err := mediapath.Resolve(s.root, key)
	if err != nil {
		return nil, fmt.Errorf("resolving object key %q: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return nil, fmt.Errorf("creating object directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(finalPath), mediapath.GeneratedAssetTempFilePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("creating object staging file: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return nil, fmt.Errorf("setting object permissions: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(temp.Name())
		return nil, fmt.Errorf("closing object staging file: %w", err)
	}
	rel, err := filepath.Rel(s.root, temp.Name())
	if err != nil {
		_ = os.Remove(temp.Name())
		return nil, fmt.Errorf("relating staging path: %w", err)
	}
	hashState, err := marshalHash(sha256.New())
	if err != nil {
		_ = os.Remove(temp.Name())
		return nil, err
	}
	return encodeUploadState(uploadState{
		Key: key, Size: size, SHA256: hex.EncodeToString(wantSHA[:]),
		Staging: filepath.ToSlash(rel), Hash: hashState,
	})
}

// ContinueUpload は staging を offset で切り詰めてから src を追記し、fsync できた分だけを状態に反映する。
func (s *Store) ContinueUpload(ctx context.Context, raw []byte, src io.Reader) ([]byte, int64, error) {
	st, err := decodeUploadState(raw)
	if err != nil {
		return raw, 0, err
	}
	f, err := os.OpenFile(s.stagingPath(st), os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return raw, st.Offset, fmt.Errorf("staging file for %q is gone: %w", st.Key, objstore.ErrUploadInvalid)
	}
	if err != nil {
		return raw, st.Offset, fmt.Errorf("opening staging file for %q: %w", st.Key, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return raw, st.Offset, fmt.Errorf("stating staging file for %q: %w", st.Key, err)
	}
	if info.Size() < st.Offset {
		return raw, st.Offset, fmt.Errorf("staging file for %q has %d bytes, want at least %d: %w", st.Key, info.Size(), st.Offset, objstore.ErrUploadInvalid)
	}
	h, err := restoreHash(st.Hash)
	if err != nil {
		return raw, st.Offset, err
	}
	// checkpoint より後ろは、落ちる前に書かれた未確定のバイトかもしれないので捨てる。
	if err := f.Truncate(st.Offset); err != nil {
		return raw, st.Offset, fmt.Errorf("truncating staging file for %q: %w", st.Key, err)
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return raw, st.Offset, fmt.Errorf("seeking staging file for %q: %w", st.Key, err)
	}

	remaining := st.Size - st.Offset
	w := &hashingFileWriter{f: f, h: h}
	_, copyErr := io.Copy(w, &contextReader{ctx: ctx, reader: io.LimitReader(src, remaining)})
	if copyErr == nil && w.n < remaining && ctx.Err() == nil {
		copyErr = fmt.Errorf("object source for %q ended after %d of %d bytes: %w", st.Key, st.Offset+w.n, st.Size, objstore.ErrChecksumMismatch)
	}
	if copyErr == nil && w.n == remaining {
		var extra [1]byte
		if n, err := io.ReadFull(&contextReader{ctx: ctx, reader: src}, extra[:]); n != 0 {
			copyErr = fmt.Errorf("object source for %q contains more than %d bytes: %w", st.Key, st.Size, objstore.ErrChecksumMismatch)
		} else if err != nil && !errors.Is(err, io.EOF) {
			copyErr = fmt.Errorf("reading object source for %q: %w", st.Key, err)
		}
	}
	if err := f.Sync(); err != nil {
		return raw, st.Offset, errors.Join(copyErr, fmt.Errorf("syncing staging file for %q: %w", st.Key, err))
	}
	// Sync できたのは w が書いた分だけ。hash も同じ分だけを含むので、offset と組で保存できる。
	st.Offset += w.n
	st.Hash, err = marshalHash(h)
	if err != nil {
		return raw, st.Offset - w.n, err
	}
	out, err := encodeUploadState(st)
	if err != nil {
		return raw, st.Offset - w.n, err
	}
	if copyErr != nil {
		return out, st.Offset, fmt.Errorf("writing object staging file for %q: %w", st.Key, copyErr)
	}
	return out, st.Offset, nil
}

// CompleteUpload は staging の長さと hash を照合し、一致したときだけ key に公開する。
// 同じ key に同じ内容があれば成功、違う内容があれば ErrAlreadyExists を返す。
func (s *Store) CompleteUpload(_ context.Context, raw []byte) error {
	st, err := decodeUploadState(raw)
	if err != nil {
		return err
	}
	if st.Offset != st.Size {
		return fmt.Errorf("upload for %q has %d of %d bytes", st.Key, st.Offset, st.Size)
	}
	want, err := decodeSHA256(st.SHA256)
	if err != nil {
		return err
	}
	h, err := restoreHash(st.Hash)
	if err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != want {
		return fmt.Errorf("upload for %q: %w", st.Key, objstore.ErrChecksumMismatch)
	}
	finalPath, err := mediapath.Resolve(s.root, st.Key)
	if err != nil {
		return fmt.Errorf("resolving object key %q: %w", st.Key, err)
	}
	stagingPath := s.stagingPath(st)
	info, err := os.Stat(stagingPath)
	if errors.Is(err, os.ErrNotExist) {
		// 公開後・staging 削除前に止まった再試行は、同じ内容の公開済みオブジェクトがあれば成功とする。
		if same, cmpErr := sameContent(finalPath, st.Size, want); cmpErr == nil && same {
			return nil
		}
		return fmt.Errorf("staging file for %q is gone: %w", st.Key, objstore.ErrUploadInvalid)
	}
	if err != nil {
		return fmt.Errorf("stating staging file for %q: %w", st.Key, err)
	}
	if info.Size() != st.Size {
		return fmt.Errorf("staging file for %q has %d bytes, want %d: %w", st.Key, info.Size(), st.Size, objstore.ErrChecksumMismatch)
	}
	if err := fsink.PublishLink(stagingPath, finalPath); err != nil {
		if errors.Is(err, fsink.ErrForeignDestination) || errors.Is(err, os.ErrExist) {
			same, cmpErr := sameContent(finalPath, st.Size, want)
			if cmpErr != nil {
				return fmt.Errorf("comparing existing object %q: %w", st.Key, cmpErr)
			}
			if !same {
				return errors.Join(objstore.ErrAlreadyExists, err)
			}
		} else {
			return fmt.Errorf("publishing object: %w", err)
		}
	}
	if err := fsink.SyncParentDir(finalPath); err != nil {
		return fmt.Errorf("syncing object directory: %w", err)
	}
	if err := os.Remove(stagingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing object staging link: %w", err)
	}
	return nil
}

// AbortUpload は staging を消す。最終パスには触れない。消せた、または既に無ければ gone を true にする。
func (s *Store) AbortUpload(_ context.Context, raw []byte) (bool, error) {
	st, err := decodeUploadState(raw)
	if err != nil {
		return false, err
	}
	path := s.stagingPath(st)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("removing staging file for %q: %w", st.Key, err)
	}
	if err := fsink.SyncParentDir(path); err != nil {
		return false, fmt.Errorf("syncing object directory after abort: %w", err)
	}
	return true, nil
}

// GetFrom は completed object の offset から読める reader を返す。
func (s *Store) GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, fmt.Errorf("negative read offset %d", offset)
	}
	f, _, err := s.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seeking object %q to %d: %w", key, offset, err)
	}
	return f, nil
}

func (s *Store) stagingPath(st uploadState) string {
	return filepath.Join(s.root, filepath.FromSlash(st.Staging))
}

// decodeUploadState は再開状態を読む。staging が接頭辞付きの一時ファイルでないものは
// 最終パスを消しかねないので拒否する。
func decodeUploadState(raw []byte) (uploadState, error) {
	var st uploadState
	if err := json.Unmarshal(raw, &st); err != nil {
		return uploadState{}, fmt.Errorf("decoding upload state: %w: %w", objstore.ErrUploadInvalid, err)
	}
	if st.Key == "" || st.Size < 0 || st.Offset < 0 || st.Offset > st.Size ||
		!filepath.IsLocal(filepath.FromSlash(st.Staging)) || !mediapath.IsGeneratedAssetTempFile(st.Staging) {
		return uploadState{}, fmt.Errorf("upload state is inconsistent: %w", objstore.ErrUploadInvalid)
	}
	return st, nil
}

func encodeUploadState(st uploadState) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("encoding upload state: %w", err)
	}
	return raw, nil
}

func decodeSHA256(hexSHA string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(hexSHA)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("upload state has an invalid SHA-256 %q: %w", hexSHA, objstore.ErrUploadInvalid)
	}
	copy(out[:], raw)
	return out, nil
}

func marshalHash(h hash.Hash) ([]byte, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("hash %T cannot be marshaled", h)
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshaling SHA-256 state: %w", err)
	}
	return state, nil
}

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if err := fsink.RestoreHash(h, state); err != nil {
		return nil, fmt.Errorf("restoring SHA-256 state: %w: %w", objstore.ErrUploadInvalid, err)
	}
	return h, nil
}

// sameContent は path が size バイトで SHA-256 が want の通常ファイルかを確かめる。
func sameContent(path string, size int64, want [32]byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	return got == want, nil
}

// hashingFileWriter は f に書けたバイトだけを h に入れる。n と h は常に同じ分を指す。
type hashingFileWriter struct {
	f *os.File
	h hash.Hash
	n int64
}

func (w *hashingFileWriter) Write(p []byte) (int, error) {
	m, err := w.f.Write(p)
	if m > 0 {
		_, _ = w.h.Write(p[:m])
		w.n += int64(m)
	}
	return m, err
}

var _ objstore.Resumable = (*Store)(nil)
var _ objstore.RangeReader = (*Store)(nil)

var _ objstore.Store = (*Store)(nil)
var _ objstore.Reader = (*Store)(nil)
var _ objstore.Local = (*Store)(nil)
var _ objstore.SignedURL = (*Store)(nil)
