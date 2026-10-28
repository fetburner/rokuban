package worker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
)

type objectCompanion struct {
	Key         string
	Path        string
	ContentType string
}

// publishGeneratedObject reserves a pending DB copy before uploading bytes, then
// calls the existing transactional publisher to activate it after the PUT.
func publishGeneratedObject(
	ctx context.Context,
	objects objstore.Store,
	publisher store.DerivedMediaPublisher,
	pending store.PendingMediaAssetCopy,
	scratchPath, contentType string,
	commit func(int64, func() error) (bool, error),
	companions ...objectCompanion,
) (int64, bool, error) {
	if objects == nil || publisher == nil {
		return 0, false, fmt.Errorf("object store and derived media publisher are required")
	}
	size, checksum, err := hashObjectFile(scratchPath)
	if err != nil {
		return 0, false, fmt.Errorf("hashing generated object %q: %w", pending.RelPath, err)
	}
	pending.SizeBytes = size
	if err := publisher.PreparePendingMediaAssetCopy(ctx, pending); err != nil {
		return 0, false, fmt.Errorf("recording pending object copy: %w", err)
	}
	if err := uploadObjectFile(ctx, objects, pending.RelPath, scratchPath, size, checksum, contentType); err != nil {
		return 0, false, err
	}
	for _, companion := range companions {
		companionSize, companionChecksum, err := hashObjectFile(companion.Path)
		if err != nil {
			return size, false, fmt.Errorf("hashing companion object %q: %w", companion.Key, err)
		}
		if err := uploadObjectFile(ctx, objects, companion.Key, companion.Path, companionSize, companionChecksum, companion.ContentType); err != nil {
			return size, false, err
		}
	}
	published, err := commit(size, func() error { return nil })
	return size, published, err
}

func hashObjectFile(path string) (int64, [sha256.Size]byte, error) {
	var checksum [sha256.Size]byte
	info, err := os.Stat(path)
	if err != nil {
		return 0, checksum, fmt.Errorf("stating generated object: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, checksum, fmt.Errorf("generated object is not a non-empty regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, checksum, fmt.Errorf("opening generated object: %w", err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	closeErr := f.Close()
	if err != nil {
		return 0, checksum, fmt.Errorf("reading generated object: %w", err)
	}
	if closeErr != nil {
		return 0, checksum, fmt.Errorf("closing generated object after hashing: %w", closeErr)
	}
	if n != info.Size() {
		return 0, checksum, fmt.Errorf("generated object size changed while hashing: read %d bytes, stat reported %d", n, info.Size())
	}
	copy(checksum[:], hash.Sum(nil))
	return info.Size(), checksum, nil
}

func uploadObjectFile(ctx context.Context, objects objstore.Store, key, path string, size int64, checksum [sha256.Size]byte, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reopening generated object %q: %w", key, err)
	}
	putErr := objects.Put(ctx, key, f, size, checksum, contentType)
	closeErr := f.Close()
	if putErr != nil {
		metrics.ObjectStoreErrors.WithLabelValues("put").Inc()
		return fmt.Errorf("putting generated object %q: %w", key, putErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing generated object %q after upload: %w", key, closeErr)
	}
	return nil
}

func objectContentType(relPath string) string {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".vtt":
		return "text/vtt; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
