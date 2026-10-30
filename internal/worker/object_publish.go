package worker

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

	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
)

type objectCompanion struct {
	Key         string
	Path        string
	ContentType string
}

const generatedObjectCleanupTimeout = 10 * time.Second

// publishGeneratedObject は派生物の置き場への公開を、予約・Put・DB 公開の順に進める。
//
//  1. pending のコピー行を commit する（pendingPrepared なら呼び出し側が済ませている）。
//  2. 宣言サイズどおりに読み切った全体の SHA-256 を照合しながら Put する。
//  3. commit が公開の tx で pending を active にし、SHA-256 を論理行へ書く。
//
// guardOperationPutObject は、guard に渡す本体の Put の前の操作名。呼び出し側がこの Put だけを見分けるのに使う。
const guardOperationPutObject = "putting generated object"

// guard は Put と DB commit の直前に呼び、メディア置き場のマウント状態を再確認する。
// commit は Put の後に呼ぶ。通常の未公開 pending は reclaim まで保持するが、
// pending row が公開中に退役し、終了時に live copy が無ければ自分の key を削除する。
func publishGeneratedObject(
	ctx context.Context,
	objects objstore.Store,
	publisher store.DerivedMediaPublisher,
	pending store.PendingMediaAssetCopy,
	scratchPath, contentType string,
	pendingPrepared bool,
	guard func(operation string) error,
	commit func(size int64, sha256 [sha256.Size]byte) (bool, error),
	companions ...objectCompanion,
) (size int64, published bool, retErr error) {
	if objects == nil || publisher == nil {
		return 0, false, fmt.Errorf("object store and derived media publisher are required")
	}
	var uploadedKeys []string
	defer func() {
		if published || len(uploadedKeys) == 0 {
			return
		}
		if cleanupErr := cleanupGeneratedObjectsIfUnowned(ctx, publisher, objects, pending.RelPath, uploadedKeys); cleanupErr != nil {
			retErr = combineGeneratedObjectCleanupError(retErr, cleanupErr)
		}
	}()
	size, checksum, err := hashObjectFile(ctx, scratchPath)
	if err != nil {
		return 0, false, fmt.Errorf("hashing generated object %q: %w", pending.RelPath, err)
	}
	pending.SizeBytes = size
	if err := requireGeneratedObjectKeyAvailable(ctx, objects, pending.RelPath); err != nil {
		return 0, false, err
	}
	for _, companion := range companions {
		if err := requireGeneratedObjectKeyAvailable(ctx, objects, companion.Key); err != nil {
			return 0, false, err
		}
	}
	if !pendingPrepared {
		if err := publisher.PreparePendingMediaAssetCopy(ctx, pending); err != nil {
			if errors.Is(err, store.ErrGenerationAdvanced) {
				return 0, false, nil
			}
			return 0, false, fmt.Errorf("recording pending object copy: %w", err)
		}
	}
	if guard != nil {
		if err := guard(guardOperationPutObject); err != nil {
			return 0, false, err
		}
	}
	putSucceeded, err := uploadObjectFileTracked(ctx, objects, pending.RelPath, scratchPath, size, checksum, contentType)
	if putSucceeded {
		uploadedKeys = append(uploadedKeys, pending.RelPath)
	}
	if err != nil {
		if errors.Is(err, objstore.ErrAlreadyExists) && !putSucceeded {
			// Another publisher won the no-replace race for this reserved key. Its
			// pending row owns the bytes and will either activate or reclaim them.
			return 0, false, nil
		}
		return 0, false, err
	}
	for _, companion := range companions {
		companionSize, companionChecksum, err := hashObjectFile(ctx, companion.Path)
		if err != nil {
			return size, false, fmt.Errorf("hashing companion object %q: %w", companion.Key, err)
		}
		if guard != nil {
			if err := guard("putting generated companion object"); err != nil {
				return size, false, err
			}
		}
		putSucceeded, err := uploadObjectFileTracked(ctx, objects, companion.Key, companion.Path, companionSize, companionChecksum, companion.ContentType)
		if putSucceeded {
			uploadedKeys = append(uploadedKeys, companion.Key)
		}
		if err != nil {
			return size, false, err
		}
	}
	if guard != nil {
		if err := guard("committing generated object"); err != nil {
			return size, false, err
		}
	}
	published, err = commit(size, checksum)
	if err != nil {
		// A stale plan can also leave a live pending row for normal reclaim. A
		// missing-row sentinel identifies a reclaim race. The deferred owner check
		// also covers failures before commit, such as a companion Put error.
		if errors.Is(err, store.ErrPendingMediaAssetCopyMissing) && !errors.Is(err, store.ErrStalePublishPlan) {
			return 0, false, nil
		}
		return size, false, err
	}
	if !published {
		return 0, false, nil
	}
	return size, published, nil
}

func cleanupGeneratedObjectsIfUnowned(ctx context.Context, publisher store.DerivedMediaPublisher, objects objstore.Store, relPath string, keys []string) error {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), generatedObjectCleanupTimeout)
	defer cancel()
	live, err := publisher.HasLiveMediaAssetCopy(checkCtx, relPath)
	if err != nil {
		return fmt.Errorf("checking ownership of uploaded generated object %q: %w", relPath, err)
	}
	if live {
		return nil
	}
	return deleteUnpublishedGeneratedObjects(ctx, objects, keys)
}

func combineGeneratedObjectCleanupError(publishErr, cleanupErr error) error {
	if publishErr == nil {
		return fmt.Errorf("cleaning up unpublished generated objects: %w", cleanupErr)
	}
	if errors.Is(publishErr, store.ErrStalePublishPlan) {
		// Do not let EncodeWorker treat a failed ownership check as a clean stale-plan replan.
		return fmt.Errorf("checking cleanup ownership after stale publish plan (%s): %w", publishErr.Error(), cleanupErr)
	}
	return errors.Join(publishErr, fmt.Errorf("cleaning up generated objects after %s: %w", publishErr.Error(), cleanupErr))
}

func deleteUnpublishedGeneratedObjects(ctx context.Context, objects objstore.Store, keys []string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), generatedObjectCleanupTimeout)
	defer cancel()
	var cleanupErr error
	// Companion subtitles are adjacent to the encoded object, so remove them first.
	for i := len(keys) - 1; i >= 0; i-- {
		if err := objects.Delete(cleanupCtx, keys[i]); err != nil {
			metrics.RecordObjectStoreError("delete", err)
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deleting generated object %q: %w", keys[i], err))
		}
	}
	return cleanupErr
}

func requireGeneratedObjectKeyAvailable(ctx context.Context, objects objstore.Store, key string) error {
	_, err := objects.Head(ctx, key)
	if err == nil {
		return fmt.Errorf("generated object key %q is already occupied: %w", key, objstore.ErrAlreadyExists)
	}
	if errors.Is(err, objstore.ErrNotFound) {
		return nil
	}
	metrics.RecordObjectStoreError("head", err)
	return fmt.Errorf("checking generated object key %q: %w", key, err)
}

func hashObjectFile(ctx context.Context, path string) (int64, [sha256.Size]byte, error) {
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
	n, err := copyWithContext(ctx, hash, f)
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

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, contextReader{ctx: ctx, r: src})
}

func uploadObjectFile(ctx context.Context, objects objstore.Store, key, path string, size int64, checksum [sha256.Size]byte, contentType string) error {
	_, err := uploadObjectFileTracked(ctx, objects, key, path, size, checksum, contentType)
	return err
}

func uploadObjectFileTracked(ctx context.Context, objects objstore.Store, key, path string, size int64, checksum [sha256.Size]byte, contentType string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("reopening generated object %q: %w", key, err)
	}
	putErr := objects.Put(ctx, key, f, size, checksum, contentType)
	closeErr := f.Close()
	if putErr != nil {
		metrics.RecordObjectStoreError("put", putErr)
		return false, fmt.Errorf("putting generated object %q: %w", key, putErr)
	}
	if closeErr != nil {
		return true, fmt.Errorf("closing generated object %q after upload: %w", key, closeErr)
	}
	return true, nil
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
