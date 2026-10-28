// Package fsstore implements objstore.Store on a strong POSIX filesystem.
package fsstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

var _ objstore.Store = (*Store)(nil)
var _ objstore.Local = (*Store)(nil)
var _ objstore.SignedURL = (*Store)(nil)
