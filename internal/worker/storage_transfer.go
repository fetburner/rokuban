package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
)

const (
	storageTransferAttemptLimit int32 = 10
	storageTransferRowLimit     int32 = 1000
	storageTransferLockSnooze         = 30 * time.Second
)

var errTransferSourceMismatch = errors.New("source copy does not match the logical media asset")

// StorageTransferReconcileWorker enqueues missing desired-store copies from current policy state.
type StorageTransferReconcileWorker struct {
	Enqueuer jobq.Enqueuer
	Storage  store.StorageTransfer
	Config   config.StorageConfig
	RowLimit int32
}

// Work enqueues desired copies after the read queries have completed.
func (w *StorageTransferReconcileWorker) Work(ctx context.Context, _ jobs.StorageTransferReconcileArgs) error {
	if w.Enqueuer == nil {
		return fmt.Errorf("storage transfer reconcile: job enqueuer is nil")
	}
	if w.Storage == nil {
		return fmt.Errorf("storage transfer reconcile: storage contract is nil")
	}
	rules, err := w.Config.OriginalRulesJSON()
	if err != nil {
		return fmt.Errorf("encoding storage original rules: %w", err)
	}
	fsStores := w.Config.FSStoreNames()
	if err := w.Storage.ClearObsoleteMediaAssetTransferAttempts(ctx, rules, fsStores, w.Config.Derived); err != nil {
		return fmt.Errorf("clearing obsolete storage transfer attempts: %w", err)
	}
	if len(fsStores) < 2 {
		metrics.StorageTransferWaitingAssets.Set(0)
		metrics.StorageTransferWaitingBytes.Set(0)
		metrics.StorageTransferExhaustedAssets.Set(0)
		return nil
	}
	fingerprint, err := w.Config.PolicyFingerprint()
	if err != nil {
		return fmt.Errorf("fingerprinting storage policy: %w", err)
	}
	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = storageTransferRowLimit
	}
	assets, byteCount, err := w.Storage.CountMediaAssetTransferCandidates(ctx, rules, fsStores, w.Config.Derived, storageTransferAttemptLimit)
	if err != nil {
		return fmt.Errorf("counting storage transfer candidates: %w", err)
	}
	metrics.StorageTransferWaitingAssets.Set(float64(assets))
	metrics.StorageTransferWaitingBytes.Set(float64(byteCount))
	exhausted, err := w.Storage.CountMediaAssetTransferExhausted(ctx, rules, fsStores, w.Config.Derived, storageTransferAttemptLimit)
	if err != nil {
		return fmt.Errorf("counting exhausted storage transfers: %w", err)
	}
	metrics.StorageTransferExhaustedAssets.Set(float64(exhausted))
	transfers, err := w.Storage.ListMediaAssetTransferCandidates(ctx, rules, fsStores, w.Config.Derived, storageTransferAttemptLimit, rowLimit)
	if err != nil {
		return fmt.Errorf("listing storage transfer candidates: %w", err)
	}
	for _, candidate := range transfers {
		if err := enqueueJob(ctx, w.Enqueuer, jobs.StorageTransferArgs{
			MediaAssetID: candidate.MediaAssetID, Store: candidate.Store, PolicyFingerprint: fingerprint,
		}); err != nil {
			slog.Error("storage transfer reconcile: failed to enqueue copy", "media_asset_id", candidate.MediaAssetID, "store", candidate.Store, "err", err)
		}
	}
	return nil
}

// StorageTransferWorker copies one logical asset between mounted FS stores.
type StorageTransferWorker struct {
	Storage      store.StorageTransfer
	Config       config.StorageConfig
	Roots        map[string]string
	ObjectStores map[string]objstore.Store
	ScratchDir   string
}

// Work verifies the policy token and mounted destination before reserving one copy operation.
func (w *StorageTransferWorker) Work(ctx context.Context, args jobs.StorageTransferArgs) error {
	if w.Storage == nil {
		return fmt.Errorf("storage transfer: storage contract is nil")
	}
	if args.MediaAssetID <= 0 || args.Store == "" {
		return fmt.Errorf("storage transfer: invalid job arguments: %w", jobq.ErrDiscard)
	}
	fingerprint, err := w.Config.PolicyFingerprint()
	if err != nil {
		return fmt.Errorf("fingerprinting storage policy: %w", err)
	}
	if args.PolicyFingerprint != fingerprint {
		metrics.StorageTransferPolicyMismatches.Inc()
		slog.Info("storage transfer: discarding job for an older policy fingerprint",
			"media_asset_id", args.MediaAssetID, "store", args.Store)
		return fmt.Errorf("storage transfer policy fingerprint changed: %w", jobq.ErrDiscard)
	}
	if cfg, ok := w.Config.Stores[args.Store]; !ok || cfg.Kind != "fs" {
		return fmt.Errorf("storage transfer: store %q is not a configured fs store: %w", args.Store, jobq.ErrDiscard)
	}
	targetRoot, ok := w.Roots[args.Store]
	if !ok || targetRoot == "" {
		metrics.StorageTransferMountMismatches.WithLabelValues(args.Store).Inc()
		return fmt.Errorf("storage transfer: target store %q is not mounted on this worker", args.Store)
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "starting storage transfer"); err != nil {
		metrics.StorageTransferMountMismatches.WithLabelValues(args.Store).Inc()
		return err
	}
	rules, err := w.Config.OriginalRulesJSON()
	if err != nil {
		return fmt.Errorf("encoding storage original rules: %w", err)
	}
	fsStores, err := w.mountedFSStores(args.Store)
	if err != nil {
		return err
	}
	return w.copyAsset(ctx, args, rules, fsStores, transferSourceStores(w.Config, fsStores), targetRoot)
}

func (w *StorageTransferWorker) mountedFSStores(targetStore string) ([]string, error) {
	var mounted []string
	for _, name := range w.Config.FSStoreNames() {
		root := w.Roots[name]
		if root == "" {
			continue
		}
		if err := requireMediaStoreMarker(root, name, "checking storage transfer mount"); err != nil {
			if name == targetStore {
				metrics.StorageTransferMountMismatches.WithLabelValues(name).Inc()
				return nil, fmt.Errorf("storage transfer: target store %q became unavailable before reservation: %w", name, err)
			}
			slog.Warn("storage transfer: skipping source store with a mismatched marker", "store", name, "err", err)
			continue
		}
		mounted = append(mounted, name)
	}
	return mounted, nil
}

func (w *StorageTransferWorker) copyAsset(ctx context.Context, args jobs.StorageTransferArgs, rules []byte, fsStores, sourceStores []string, targetRoot string) (retErr error) {
	transfer, ok, err := w.Storage.PrepareMediaAssetTransfer(
		ctx, args.MediaAssetID, args.Store, rules, fsStores, sourceStores, w.Config.Derived, storageTransferAttemptLimit,
	)
	if err != nil || !ok {
		return err
	}
	log := slog.With("media_asset_id", transfer.MediaAssetID, "source_store", transfer.SourceStore, "target_store", args.Store)
	sourceRoot := w.Roots[transfer.SourceStore]
	lease, acquired, err := w.Storage.AcquireTransferSourceBandwidth(ctx, transfer.SourceStore)
	if err != nil {
		return fmt.Errorf("acquiring transfer source bandwidth lock: %w", err)
	}
	if !acquired {
		return jobq.Snooze(storageTransferLockSnooze)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lease.Release(releaseCtx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("releasing transfer source bandwidth lock: %w", err))
		}
	}()
	if transfer.Kind != store.AssetKindOriginal {
		return w.copyDerivedAsset(ctx, transfer, args, rules, targetRoot, log)
	}
	src, err := w.originalTransferSource(transfer)
	if err != nil {
		return err
	}
	targetPath, err := mediapath.Resolve(targetRoot, transfer.RelPath)
	if err != nil {
		return fmt.Errorf("resolving transfer destination: %w", err)
	}
	paths := []transferPathLock{{store: args.Store, root: targetRoot, relPath: transfer.RelPath}}
	// リモートの転送元には、ロックを取る先のローカル root が無い。ローカルの原本だけ rel_path lock を取る。
	if sourceRoot != "" {
		paths = append(paths, transferPathLock{store: transfer.SourceStore, root: sourceRoot, relPath: transfer.SourceRelPath})
	}
	locks, err := lockTransferPaths(ctx, paths)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return w.recordTransferFailure(ctx, transfer, log, err)
	}
	defer closeTransferLocks(locks)
	if err := requireMediaStoreMarker(targetRoot, args.Store, "writing storage transfer destination"); err != nil {
		return err
	}
	if sourceRoot != "" {
		if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "reading storage transfer source"); err != nil {
			return err
		}
	}

	if handled, err := w.adoptExistingDestination(ctx, transfer, args, rules, targetPath, src, log); handled {
		return err
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "creating transfer destination directory"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("creating transfer destination directory: %w", err))
	}
	tempPath := transferTempPath(targetPath, transfer.MediaAssetID, transfer.CopyID)
	tempLock, created, err := fsink.LockTempFile(tempPath)
	if err != nil {
		if fsink.TempLockBusy(err) {
			return nil
		}
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("locking transfer temp: %w", err))
	}
	defer func() { _ = tempLock.Close() }()
	if created {
		if err := os.Chmod(tempPath, 0o644); err != nil {
			return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("setting transfer temp mode: %w", err))
		}
	}
	digest, size, paused, copyErr := w.copyToTransferTemp(ctx, src, tempPath, tempLock, transfer.SourceStore, args.Store, transfer.SizeBytes, len(transfer.SHA256) > 0)
	if paused {
		return nil
	}
	if errors.Is(copyErr, errTransferSourceMismatch) {
		return w.handleTransferMismatch(ctx, transfer, log, src, tempPath, "transfer temp prefix differs from source")
	}
	if copyErr != nil {
		return w.recordTransferFailure(ctx, transfer, log, copyErr)
	}
	if size != transfer.SizeBytes {
		return w.handleTransferMismatch(ctx, transfer, log, src, tempPath, fmt.Sprintf("source size %d differs from logical asset size %d", size, transfer.SizeBytes))
	}
	if len(transfer.SHA256) > 0 && !bytes.Equal(digest, transfer.SHA256) {
		return w.handleTransferMismatch(ctx, transfer, log, src, tempPath, "transfer SHA-256 differs from logical asset")
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "publishing storage transfer"); err != nil {
		return err
	}
	if err := fsink.PublishLink(tempPath, targetPath); err != nil {
		return w.recordTransferFailure(ctx, transfer, log, err)
	}
	if err := fsink.SyncParentDir(targetPath); err != nil {
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("syncing transfer destination directory: %w", err))
	}
	return w.completeTransfer(ctx, transfer, args, rules, digest, src, targetPath, tempPath, true, log)
}

// copyDerivedAsset transfers derived media through the object-store API. Temporary bytes live only in scratch;
// canonical keys are published once with Store.Put and become visible when CompleteMediaAssetTransfer commits.
func (w *StorageTransferWorker) copyDerivedAsset(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules []byte, targetRoot string, log *slog.Logger) error {
	sourceStore := w.ObjectStores[transfer.SourceStore]
	targetStore := w.ObjectStores[args.Store]
	if sourceStore == nil || targetStore == nil {
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("derived transfer object stores are not configured for %q -> %q", transfer.SourceStore, args.Store))
	}
	if sourceRoot := w.Roots[transfer.SourceStore]; sourceRoot != "" {
		if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "reading derived storage transfer source"); err != nil {
			return err
		}
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "writing derived storage transfer destination"); err != nil {
		return err
	}
	// scratch は、内容を複製して照合する必要があるときだけ作る（SHA-256 が既知の本体と、字幕が無い encoded は作らない）。
	var scratchPath string
	defer func() {
		if scratchPath != "" {
			_ = os.RemoveAll(scratchPath)
		}
	}()
	scratchDir := func() (string, error) {
		if scratchPath != "" {
			return scratchPath, nil
		}
		path, err := newWorkerScratchDir(w.ScratchDir, "storage-transfer", fmt.Sprintf("derived-%d-%d-", transfer.MediaAssetID, transfer.CopyID))
		if err != nil {
			return "", fmt.Errorf("creating derived transfer scratch: %w", err)
		}
		scratchPath = path
		return path, nil
	}
	ownedRelPaths, err := w.Storage.ListMediaAssetTransferObjectsOwned(ctx, transfer.CopyID, args.Store)
	if err != nil {
		return fmt.Errorf("listing owned derived transfer objects: %w", err)
	}
	owned := make(map[string]struct{}, len(ownedRelPaths))
	for _, relPath := range ownedRelPaths {
		owned[relPath] = struct{}{}
	}

	// 論理資産の SHA-256 が既知なら、source の Get を（帯域制限を通して）そのまま Put に流す。
	// Store.Put は宣言サイズと SHA-256 が合わなければ何も公開しないので、scratch に複製して照合する必要が無い。
	// 未計測のときだけ scratch に複製してハッシュを取る。
	var openMain func() (io.ReadCloser, error)
	digest, size := transfer.SHA256, transfer.SizeBytes
	var streamed *countingReadCloser
	if len(transfer.SHA256) == sha256.Size {
		openMain = func() (io.ReadCloser, error) {
			source, err := w.openTransferSourceStream(ctx, sourceStore, transfer.SourceStore, transfer.SourceRelPath)
			if err != nil {
				return nil, err
			}
			streamed = source
			return source, nil
		}
	} else {
		assetPath, stagedDigest, stagedSize, err := w.stageObjectForTransfer(ctx, sourceStore, transfer.SourceStore, transfer.SourceRelPath, scratchDir, transfer.SizeBytes, transfer.SHA256)
		if err != nil {
			if errors.Is(err, errTransferSourceMismatch) {
				return w.handleTransferMismatch(ctx, transfer, log, nil, "", "derived source differs from logical asset metadata")
			}
			return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("reading derived transfer source: %w", err))
		}
		digest, size = stagedDigest, stagedSize
		openMain = func() (io.ReadCloser, error) { return os.Open(assetPath) }
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "publishing derived storage transfer destination"); err != nil {
		return err
	}
	_, mainOwned := owned[transfer.RelPath]
	mainCreated, err := putTransferredObject(ctx, targetStore, transfer.RelPath, openMain, size, digest, objectContentType(transfer.RelPath), mainOwned)
	if err != nil {
		if streamed != nil && streamed.sourceDiffers(size, err) {
			return w.handleTransferMismatch(ctx, transfer, log, nil, "", "derived source differs from logical asset metadata")
		}
		metrics.RecordObjectStoreError("put", err)
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("publishing derived transfer destination: %w", err))
	}
	if mainCreated {
		pending, err := w.Storage.MarkMediaAssetTransferObjectOwned(ctx, transfer.CopyID, args.Store, transfer.RelPath)
		if err != nil {
			cleanupErr := targetStore.Delete(ctx, transfer.RelPath)
			return w.recordTransferFailure(ctx, transfer, log, errors.Join(
				fmt.Errorf("recording ownership of derived transfer destination: %w", err), cleanupErr,
			))
		}
		if !pending {
			if err := targetStore.Delete(ctx, transfer.RelPath); err != nil {
				return fmt.Errorf("removing destination created after transfer was retired: %w", err)
			}
			return nil
		}
		owned[transfer.RelPath] = struct{}{}
	}

	continueTransfer, err := w.syncDerivedTransferSubtitle(ctx, transfer, args, sourceStore, targetStore, scratchDir, owned, log)
	if err != nil {
		return err
	}
	if !continueTransfer {
		return nil
	}

	if err := requireMediaStoreMarker(targetRoot, args.Store, "committing derived storage transfer"); err != nil {
		return err
	}
	active, rejection, err := w.Storage.CompleteMediaAssetTransfer(ctx, transfer, args.Store, w.Config.Derived, rules, digest)
	if err != nil {
		return fmt.Errorf("committing derived storage transfer: %w", err)
	}
	if !active {
		if err := deleteOwnedTransferObjects(ctx, targetStore, transfer.RelPath, transfer.Kind, ownedRelPathsFromSet(owned), nil); err != nil {
			return fmt.Errorf("removing uncommitted derived transfer objects: %w", err)
		}
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.CopyID); err != nil {
			return err
		}
		if rejection != "" {
			return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("derived transfer completion rejected: %s", rejection))
		}
		return nil
	}
	metrics.StorageTransferLastSuccess.Set(float64(time.Now().Unix()))
	log.Info("storage transfer: derived copy active", "bytes", size)
	return nil
}

func (w *StorageTransferWorker) syncDerivedTransferSubtitle(
	ctx context.Context,
	transfer store.MediaAssetTransfer,
	args jobs.StorageTransferArgs,
	sourceStore, targetStore objstore.Store,
	scratchDir func() (string, error),
	owned map[string]struct{},
	log *slog.Logger,
) (bool, error) {
	if transfer.Kind != store.AssetKindEncoded || !canHaveSubtitleSidecar(transfer.RelPath) {
		return true, nil
	}
	sourceSubtitleKey, err := mediapath.SubtitleSibling(transfer.SourceRelPath)
	if err != nil {
		return false, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("building source subtitle object key: %w", err))
	}
	subtitleKey, err := mediapath.SubtitleSibling(transfer.RelPath)
	if err != nil {
		return false, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("building destination subtitle object key: %w", err))
	}
	subtitlePath, subtitleDigest, subtitleSize, subtitleErr := w.stageObjectForTransfer(ctx, sourceStore, transfer.SourceStore, sourceSubtitleKey, scratchDir, -1, nil)
	if errors.Is(subtitleErr, objstore.ErrNotFound) {
		// source に字幕が無いとき、宛先の字幕 key は所有記録のある（この転送が作った）ものだけ消す。
		// 所有記録の無い key は、この転送が作ったものではないので残す。
		if _, subtitleOwned := owned[subtitleKey]; subtitleOwned {
			if err := targetStore.Delete(ctx, subtitleKey); err != nil {
				return false, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("removing absent transferred subtitle: %w", err))
			}
			if err := w.Storage.DeleteMediaAssetTransferObjectOwned(ctx, transfer.CopyID, args.Store, subtitleKey); err != nil {
				return false, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("clearing absent transferred subtitle ownership: %w", err))
			}
			delete(owned, subtitleKey)
		}
		return true, nil
	}
	if subtitleErr != nil {
		return false, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("reading transferred subtitle: %w", subtitleErr))
	}
	_, subtitleOwned := owned[subtitleKey]
	subtitleCreated, err := putTransferredObject(ctx, targetStore, subtitleKey, func() (io.ReadCloser, error) { return os.Open(subtitlePath) }, subtitleSize, subtitleDigest, "text/vtt; charset=utf-8", subtitleOwned)
	if err != nil {
		metrics.RecordObjectStoreError("put", err)
		return false, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("publishing transferred subtitle: %w", err))
	}
	if !subtitleCreated {
		return true, nil
	}
	pending, err := w.Storage.MarkMediaAssetTransferObjectOwned(ctx, transfer.CopyID, args.Store, subtitleKey)
	if err != nil {
		cleanupErr := targetStore.Delete(ctx, subtitleKey)
		return false, w.recordTransferFailure(ctx, transfer, log, errors.Join(
			fmt.Errorf("recording ownership of transferred subtitle: %w", err), cleanupErr,
		))
	}
	if !pending {
		if err := targetStore.Delete(ctx, subtitleKey); err != nil {
			return false, fmt.Errorf("removing subtitle created after transfer was retired: %w", err)
		}
		return false, nil
	}
	owned[subtitleKey] = struct{}{}
	return true, nil
}

func (w *StorageTransferWorker) stageObjectForTransfer(ctx context.Context, objects objstore.Store, sourceStoreName, key string, scratchDir func() (string, error), expectedSize int64, expectedSHA []byte) (string, []byte, int64, error) {
	reader, ok := objects.(objstore.Reader)
	if !ok {
		return "", nil, 0, fmt.Errorf("object store %q does not support reads", sourceStoreName)
	}
	source, err := reader.Get(ctx, key)
	if err != nil {
		return "", nil, 0, err
	}
	dir, err := scratchDir()
	if err != nil {
		_ = source.Close()
		return "", nil, 0, err
	}
	path, err := os.CreateTemp(dir, "object-*")
	if err != nil {
		_ = source.Close()
		return "", nil, 0, fmt.Errorf("creating scratch object: %w", err)
	}
	name := path.Name()
	hasher := sha256.New()
	limited, err := w.transferSourceReader(ctx, sourceStoreName, source)
	if err != nil {
		_ = source.Close()
		_ = path.Close()
		_ = os.Remove(name)
		return "", nil, 0, err
	}
	count, copyErr := io.CopyBuffer(io.MultiWriter(path, hasher), limited, make([]byte, 64*1024))
	closeSourceErr := source.Close()
	closeFileErr := path.Close()
	if copyErr != nil {
		_ = os.Remove(name)
		return "", nil, count, fmt.Errorf("copying object to scratch: %w", copyErr)
	}
	if closeSourceErr != nil {
		_ = os.Remove(name)
		return "", nil, count, fmt.Errorf("closing source object: %w", closeSourceErr)
	}
	if closeFileErr != nil {
		_ = os.Remove(name)
		return "", nil, count, fmt.Errorf("closing scratch object: %w", closeFileErr)
	}
	digest := hasher.Sum(nil)
	if expectedSize >= 0 && count != expectedSize {
		_ = os.Remove(name)
		return "", nil, count, fmt.Errorf("source object size is %d, want %d: %w", count, expectedSize, errTransferSourceMismatch)
	}
	if len(expectedSHA) != 0 && !bytes.Equal(digest, expectedSHA) {
		_ = os.Remove(name)
		return "", nil, count, fmt.Errorf("source object SHA-256 differs from logical asset: %w", errTransferSourceMismatch)
	}
	return name, digest, count, nil
}

// putTransferredObject は key が無ければ open が返す内容を Put する。既に内容の同じ key があれば何もせず、
// 内容の違う既存の key は所有記録の有無にかかわらず衝突として扱い、消さない。
func putTransferredObject(ctx context.Context, objects objstore.Store, key string, open func() (io.ReadCloser, error), size int64, digest []byte, contentType string, alreadyOwned bool) (created bool, err error) {
	var checksum [sha256.Size]byte
	if len(digest) != len(checksum) {
		return false, fmt.Errorf("invalid SHA-256 length %d", len(digest))
	}
	copy(checksum[:], digest)
	if _, headErr := objects.Head(ctx, key); headErr == nil {
		actual, actualSize, err := hashStoredObject(ctx, objects, key)
		if err != nil {
			return false, err
		}
		if actualSize == size && bytes.Equal(actual, digest) {
			return false, nil
		}
		ownership := "pre-existing"
		if alreadyOwned {
			ownership = "owned"
		}
		return false, fmt.Errorf("%s destination object differs from transfer source: %w", ownership, objstore.ErrAlreadyExists)
	} else if !errors.Is(headErr, objstore.ErrNotFound) {
		return false, headErr
	}
	src, err := open()
	if err != nil {
		return false, err
	}
	putErr := objects.Put(ctx, key, src, size, checksum, contentType)
	closeErr := src.Close()
	if putErr != nil {
		return false, fmt.Errorf("putting transferred object %q: %w", key, putErr)
	}
	if closeErr != nil {
		return true, fmt.Errorf("closing transfer source for %q: %w", key, closeErr)
	}
	return true, nil
}

// countingReadCloser は、Put に流す source の読んだ長さと読み取りエラーを覚える。
type countingReadCloser struct {
	io.Reader
	closer  io.Closer
	n       int64
	eof     bool
	readErr error
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += int64(n)
	if errors.Is(err, io.EOF) {
		c.eof = true
	} else if err != nil {
		c.readErr = err
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.closer.Close() }

// sourceDiffers は Put の失敗が、source の内容が論理資産と違うことによるかを返す。
// SHA-256 の不一致のほか、source が宣言サイズより短く終わった場合と、宣言サイズを超えて続く場合も含む。
// source の読み取り自体が失敗したときや、書き込み側の失敗で途中までしか読んでいないときは、
// 内容が違うとは言えないので false を返す。
func (c *countingReadCloser) sourceDiffers(size int64, putErr error) bool {
	if c.readErr != nil {
		return false
	}
	return errors.Is(putErr, objstore.ErrChecksumMismatch) || (c.eof && c.n != size) || c.n > size
}

// openTransferSourceStream は source の Get を、帯域制限を通した読み取りとして開く。
func (w *StorageTransferWorker) openTransferSourceStream(ctx context.Context, objects objstore.Store, sourceStoreName, key string) (*countingReadCloser, error) {
	reader, ok := objects.(objstore.Reader)
	if !ok {
		return nil, fmt.Errorf("object store %q does not support reads", sourceStoreName)
	}
	source, err := reader.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	limited, err := w.transferSourceReader(ctx, sourceStoreName, source)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	return &countingReadCloser{Reader: limited, closer: source}, nil
}

func ownedRelPathsFromSet(owned map[string]struct{}) []string {
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	return paths
}

func deleteOwnedTransferObjects(ctx context.Context, objects objstore.Store, relPath, kind string, ownedRelPaths []string, beforeDelete func() error) error {
	allowed := map[string]struct{}{relPath: {}}
	sidecar := ""
	if kind == store.AssetKindEncoded && canHaveSubtitleSidecar(relPath) {
		var err error
		sidecar, err = mediapath.SubtitleSibling(relPath)
		if err != nil {
			return fmt.Errorf("building owned subtitle key: %w", err)
		}
		allowed[sidecar] = struct{}{}
	}
	owned := make(map[string]struct{}, len(ownedRelPaths))
	for _, key := range ownedRelPaths {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unexpected owned transfer key %q", key)
		}
		owned[key] = struct{}{}
	}
	keys := make([]string, 0, 2)
	if sidecar != "" {
		if _, ok := owned[sidecar]; ok {
			keys = append(keys, sidecar)
		}
	}
	if _, ok := owned[relPath]; ok {
		keys = append(keys, relPath)
	}
	for _, key := range keys {
		if beforeDelete != nil {
			if err := beforeDelete(); err != nil {
				return err
			}
		}
		if err := objects.Delete(ctx, key); err != nil {
			return fmt.Errorf("deleting owned transfer object %q: %w", key, err)
		}
	}
	return nil
}

func hashStoredObject(ctx context.Context, objects objstore.Store, key string) ([]byte, int64, error) {
	reader, ok := objects.(objstore.Reader)
	if !ok {
		return nil, 0, fmt.Errorf("object store does not support reads")
	}
	object, err := reader.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = object.Close() }()
	hasher := sha256.New()
	n, err := copyWithContext(ctx, hasher, object)
	if err != nil {
		return nil, n, err
	}
	return hasher.Sum(nil), n, nil
}

// transferTempPath は、1 つの宛先コピーの転送が使う temp のパスを返す。
func transferTempPath(targetPath string, mediaAssetID, copyID int64) string {
	return filepath.Join(filepath.Dir(targetPath), fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, mediaAssetID, copyID))
}

func (w *StorageTransferWorker) adoptExistingDestination(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules []byte, targetPath string, src transferSource, log *slog.Logger) (bool, error) {
	_, statErr := os.Lstat(targetPath)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if statErr != nil {
		return true, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("stating transfer destination: %w", statErr))
	}
	// link 済み・commit 前に落ちた転送は、宛先と temp が同じ inode を指す。その temp と checkpoint は
	// 採用と同時に片付ける。他人の宛先を採用するときは temp に触れない。
	ownedTempPath := ""
	tempPath := transferTempPath(targetPath, transfer.MediaAssetID, transfer.CopyID)
	owned, err := pendingTransferDestinationOwnedByTemp(targetPath, tempPath)
	if err != nil {
		return true, w.recordTransferFailure(ctx, transfer, log, err)
	}
	if owned {
		ownedTempPath = tempPath
	}
	destinationSHA, size, err := hashTransferFile(ctx, targetPath)
	if err != nil {
		return true, w.recordTransferFailure(ctx, transfer, log, err)
	}
	if size == transfer.SizeBytes && len(transfer.SHA256) > 0 && bytes.Equal(destinationSHA, transfer.SHA256) {
		return true, w.completeTransfer(ctx, transfer, args, rules, destinationSHA, src, targetPath, ownedTempPath, false, log)
	}
	if size == transfer.SizeBytes && len(transfer.SHA256) == 0 {
		// 論理資産の SHA-256 が未知でリモートの転送元なら、既存の宛先を照合できない。採用せず失敗として記録する。
		// 通常の copy に進むと PublishLink が既存の宛先を拒むので、その経路にも落とさない。
		if _, remote := src.(remoteTransferSource); remote {
			return true, w.recordTransferFailure(ctx, transfer, log, errors.New(
				"destination already exists, the source is remote, and the logical SHA-256 is unknown, so the destination cannot be verified"))
		}
		sourceSHA, sourceSize, sourceErr := w.hashTransferSource(ctx, src, transfer.SourceStore)
		if sourceErr != nil {
			return true, w.recordTransferFailure(ctx, transfer, log, sourceErr)
		}
		if sourceSize != transfer.SizeBytes {
			return true, w.handleTransferMismatch(ctx, transfer, log, src, "", "source size differs from logical asset")
		}
		if bytes.Equal(destinationSHA, sourceSHA) {
			return true, w.completeTransfer(ctx, transfer, args, rules, sourceSHA, src, targetPath, ownedTempPath, false, log)
		}
	}
	return true, w.handleTransferMismatch(ctx, transfer, log, src, "", fmt.Sprintf("destination already exists with different size or SHA-256 (size=%d, want=%d)", size, transfer.SizeBytes))
}

func (w *StorageTransferWorker) copyToTransferTemp(ctx context.Context, src transferSource, tempPath string, tempLock *os.File, sourceStore, targetStore string, expectedSize int64, sourceSHAKnown bool) ([]byte, int64, bool, error) {
	sourceSize, err := src.precheck()
	if err != nil {
		return nil, 0, false, err
	}
	if sourceSize >= 0 && sourceSize != expectedSize {
		return nil, sourceSize, false, errTransferSourceMismatch
	}
	if err := resetOversizedTransferTemp(tempPath, tempLock, expectedSize); err != nil {
		return nil, 0, false, err
	}
	paused, err := verifyCheckpointedTransferTemp(ctx, tempPath, tempLock)
	if paused || err != nil {
		return nil, 0, paused, err
	}
	hasher := sha256.New()
	offset, replayComplete, err := replayTransferTemp(ctx, tempPath, tempLock, w.Roots[targetStore], targetStore, hasher)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, true, nil //nolint:nilerr // Shutdown cancellation pauses the transfer instead of recording a failure.
		}
		return nil, 0, false, fmt.Errorf("replaying transfer temp: %w", err)
	}
	if !replayComplete || offset > expectedSize {
		return nil, offset, false, fmt.Errorf("invalid transfer checkpoint offset %d for source size %d", offset, expectedSize)
	}
	// SHA-256 が既知なら、残りだけを offset から開く（先頭の読み直しが無い）。未知なら先頭から開いて接頭辞を照合する。
	openAt := offset
	if !sourceSHAKnown {
		openAt = 0
	}
	reader, err := src.open(ctx, openAt)
	if err != nil {
		return nil, offset, false, fmt.Errorf("opening transfer source: %w", err)
	}
	defer func() { _ = reader.Close() }()
	sourceReader, err := w.transferSourceReader(ctx, sourceStore, reader)
	if err != nil {
		return nil, offset, false, err
	}
	paused, err = verifyTransferSourcePrefix(ctx, sourceReader, offset, sourceSHAKnown, hasher)
	if paused || err != nil {
		return nil, offset, paused, err
	}
	written, paused, err := w.copyTransferSourceRemainder(ctx, sourceReader, tempPath, tempLock, targetStore, offset, expectedSize, hasher)
	if paused || err != nil {
		return nil, written, paused, err
	}
	if written != expectedSize {
		return hasher.Sum(nil), written, false, nil
	}
	if err := writeFinalTransferCheckpoint(tempPath, targetStore, written, hasher, w.Roots); err != nil {
		return nil, written, false, err
	}
	return hasher.Sum(nil), written, false, nil
}

func resetOversizedTransferTemp(tempPath string, tempLock *os.File, expectedSize int64) error {
	tempInfo, err := tempLock.Stat()
	if err != nil {
		return fmt.Errorf("stating transfer temp: %w", err)
	}
	if tempInfo.Size() > expectedSize {
		if err := fsink.RemoveCheckpoint(tempPath); err != nil {
			return fmt.Errorf("removing checkpoint for oversized transfer temp: %w", err)
		}
		if err := tempLock.Truncate(0); err != nil {
			return fmt.Errorf("truncating oversized transfer temp: %w", err)
		}
	}
	if _, err := tempLock.Stat(); err != nil {
		return fmt.Errorf("stating transfer temp after size check: %w", err)
	}
	return nil
}

func verifyCheckpointedTransferTemp(ctx context.Context, tempPath string, tempLock *os.File) (bool, error) {
	tempInfo, err := tempLock.Stat()
	if err != nil {
		return false, fmt.Errorf("stating transfer temp after size check: %w", err)
	}
	checkpointHasher := sha256.New()
	checkpointOffset, checkpointed := fsink.RestoreCheckpoint(tempPath, tempInfo.Size(), checkpointHasher)
	if !checkpointed || checkpointOffset == 0 {
		return false, nil
	}
	tempPrefixHasher := sha256.New()
	tempPrefixReader := io.NewSectionReader(tempLock, 0, checkpointOffset)
	verifiedPrefixBytes, prefixErr := io.CopyN(tempPrefixHasher, &storageTransferContextReader{ctx: ctx, reader: tempPrefixReader}, checkpointOffset)
	if ctx.Err() != nil {
		return true, nil //nolint:nilerr // Shutdown cancellation pauses the transfer instead of recording a failure.
	}
	if prefixErr != nil {
		return false, fmt.Errorf("verifying checkpointed transfer temp prefix: %w", prefixErr)
	}
	if verifiedPrefixBytes != checkpointOffset {
		return false, fmt.Errorf("verifying checkpointed transfer temp prefix: read=%d expected=%d", verifiedPrefixBytes, checkpointOffset)
	}
	if bytes.Equal(tempPrefixHasher.Sum(nil), checkpointHasher.Sum(nil)) {
		return false, nil
	}
	if err := fsink.RemoveCheckpoint(tempPath); err != nil {
		return false, fmt.Errorf("removing checkpoint for damaged transfer temp: %w", err)
	}
	if err := tempLock.Truncate(0); err != nil {
		return false, fmt.Errorf("truncating damaged transfer temp: %w", err)
	}
	return false, nil
}

func replayTransferTemp(ctx context.Context, tempPath string, tempLock *os.File, targetRoot, targetStore string, hasher hash.Hash) (int64, bool, error) {
	return fsink.ReplayTempFileWithCheckpoint(ctx, tempPath, tempLock, hasher, func() error {
		return requireMediaStoreMarker(targetRoot, targetStore, "saving transfer checkpoint")
	})
}

func verifyTransferSourcePrefix(ctx context.Context, sourceReader io.Reader, offset int64, sourceSHAKnown bool, tempHasher hash.Hash) (bool, error) {
	if offset == 0 {
		return false, nil
	}
	if sourceSHAKnown {
		// 呼び出し側が offset から開いているので、ここでは読まない。
		return false, nil
	}
	sourcePrefixHasher := sha256.New()
	_, err := io.CopyN(sourcePrefixHasher, sourceReader, offset)
	if err != nil {
		if ctx.Err() != nil {
			return true, nil //nolint:nilerr // Shutdown cancellation pauses the transfer instead of recording a failure.
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, errTransferSourceMismatch
		}
		return false, fmt.Errorf("hashing current transfer source prefix: %w", err)
	}
	if !bytes.Equal(sourcePrefixHasher.Sum(nil), tempHasher.Sum(nil)) {
		return false, errTransferSourceMismatch
	}
	return false, nil
}

func (w *StorageTransferWorker) copyTransferSourceRemainder(ctx context.Context, sourceReader io.Reader, tempPath string, tempLock *os.File, targetStore string, offset, expectedSize int64, hasher hash.Hash) (int64, bool, error) {
	writer, err := fsink.OpenFileFromLock(tempLock)
	if err != nil {
		return offset, false, fmt.Errorf("duplicating transfer temp descriptor: %w", err)
	}
	count, copyErr := io.CopyBuffer(fsink.NewHashingWriter(writer, hasher), io.LimitReader(sourceReader, expectedSize-offset), make([]byte, 128*1024))
	closeErr := writer.Close()
	written := offset + count
	if copyErr != nil {
		if ctx.Err() != nil {
			if err := fsink.PersistCheckpoint(tempPath, tempLock, written, hasher, func() error {
				return requireMediaStoreMarker(w.Roots[targetStore], targetStore, "persisting transfer checkpoint")
			}); err != nil {
				return written, false, fmt.Errorf("persisting interrupted transfer: %w", err)
			}
			return written, true, nil
		}
		return written, false, fmt.Errorf("copying transfer source: %w", copyErr)
	}
	if closeErr != nil {
		return written, false, fmt.Errorf("closing transfer temp writer: %w", closeErr)
	}
	if err := tempLock.Sync(); err != nil {
		return written, false, fmt.Errorf("syncing transfer temp: %w", err)
	}
	return written, false, nil
}

func writeFinalTransferCheckpoint(tempPath, targetStore string, written int64, hasher hash.Hash, roots map[string]string) error {
	if err := requireMediaStoreMarker(roots[targetStore], targetStore, "writing transfer checkpoint"); err != nil {
		return err
	}
	if err := fsink.WriteCheckpoint(tempPath, written, hasher, nil); err != nil {
		return fmt.Errorf("writing final transfer checkpoint: %w", err)
	}
	return nil
}

func (w *StorageTransferWorker) completeTransfer(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules, digest []byte, src transferSource, targetPath, tempPath string, createdCanonical bool, log *slog.Logger) error {
	if err := requireMediaStoreMarker(w.Roots[args.Store], args.Store, "committing transferred media asset"); err != nil {
		return err
	}
	active, rejection, err := w.Storage.CompleteMediaAssetTransfer(ctx, transfer, args.Store, w.Config.Derived, rules, digest)
	if err != nil {
		return err
	}
	if !active {
		if rejection == "sha_mismatch" {
			if createdCanonical {
				if err := requireMediaStoreMarker(w.Roots[args.Store], args.Store, "removing transfer rejected by a newer SHA-256 observation"); err != nil {
					return err
				}
				if err := os.Remove(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("removing transfer rejected by a newer SHA-256 observation: %w", err)
				}
				if err := fsink.SyncParentDir(targetPath); err != nil {
					return fmt.Errorf("syncing transfer directory after SHA-256 rejection: %w", err)
				}
			}
			return w.handleTransferMismatch(ctx, transfer, log, src, tempPath, "completion rejected a stale SHA-256")
		}
		if createdCanonical {
			if err := requireMediaStoreMarker(w.Roots[args.Store], args.Store, "removing uncommitted transfer"); err != nil {
				return err
			}
			if err := os.Remove(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("removing uncommitted transfer destination: %w", err)
			}
			if err := fsink.SyncParentDir(targetPath); err != nil {
				return fmt.Errorf("syncing directory after removing uncommitted transfer: %w", err)
			}
		}
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.CopyID); err != nil {
			return err
		}
		if tempPath != "" {
			if err := fsink.RemoveTemp(tempPath); err != nil {
				log.Warn("storage transfer: removing rejected transfer temp failed", "path", tempPath, "err", err)
			}
		}
		return nil
	}
	if tempPath != "" {
		if err := requireMediaStoreMarker(w.Roots[args.Store], args.Store, "removing committed transfer temp"); err != nil {
			log.Warn("storage transfer: skipping temp cleanup because store marker changed", "err", err)
		} else if err := fsink.RemoveTemp(tempPath); err != nil {
			log.Warn("storage transfer: removing committed temp failed", "path", tempPath, "err", err)
		}
	}
	metrics.StorageTransferLastSuccess.Set(float64(time.Now().Unix()))
	log.Info("storage transfer: copy active", "bytes", transfer.SizeBytes)
	return nil
}

func (w *StorageTransferWorker) handleTransferMismatch(ctx context.Context, transfer store.MediaAssetTransfer, log *slog.Logger, src transferSource, tempPath, reason string) error {
	var sourceSHA []byte
	var sourceSize int64
	var err error
	if transfer.Kind == store.AssetKindOriginal {
		sourceSHA, sourceSize, err = w.hashTransferSource(ctx, src, transfer.SourceStore)
	} else {
		objects := w.ObjectStores[transfer.SourceStore]
		if objects == nil {
			return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("derived source object store %q is not configured", transfer.SourceStore))
		}
		sourceSHA, sourceSize, err = hashStoredObject(ctx, objects, transfer.SourceRelPath)
	}
	if err != nil {
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("rechecking transfer source after %s: %w", reason, err))
	}
	if sourceMatchesTransferMetadata(transfer, sourceSize, sourceSHA) {
		if tempPath != "" {
			if err := fsink.RemoveTemp(tempPath); err != nil {
				return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("discarding transfer temp that differs from the valid source: %w", err))
			}
		}
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("destination differs from the valid source: %s", reason))
	}
	if transfer.Kind == "original" {
		log.Error("storage transfer: original source does not match its logical asset; preserving source",
			"reason", reason, "sha256", fmt.Sprintf("%x", sourceSHA), "size", sourceSize)
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("%w: %s", errTransferSourceMismatch, reason))
	}

	corrupt, err := w.Storage.MarkMediaAssetCopyCorrupt(ctx, transfer.RecordingID, transfer.MediaAssetID, transfer.SourceCopyID, transfer.SourceStore)
	if err != nil {
		return err
	}
	if tempPath != "" {
		if err := fsink.RemoveTemp(tempPath); err != nil {
			log.Warn("storage transfer: removing mismatched temp failed", "path", tempPath, "err", err)
		}
	}
	if corrupt {
		if err := w.unlinkRetiredDerivedSource(ctx, transfer); err != nil {
			if errors.Is(err, mediapath.ErrStoreMarkerMismatch) {
				return err
			}
			return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("removing corrupt derived source: %w", err))
		}
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.CopyID); err != nil {
			return err
		}
		metrics.StorageTransferCorruptSourcesRetired.Inc()
		log.Error("storage transfer: retired corrupt derived source for recreation", "reason", reason, "sha256", fmt.Sprintf("%x", sourceSHA))
		return nil
	}
	log.Error("storage transfer: derived source is inconsistent but no present original copy can rebuild it",
		"reason", reason, "sha256", fmt.Sprintf("%x", sourceSHA))
	return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("derived source mismatch without a present original: %s", reason))
}

// unlinkRetiredDerivedSource は破損した派生物コピーを置き場の API で消し、deleted に確定する。
func (w *StorageTransferWorker) unlinkRetiredDerivedSource(ctx context.Context, transfer store.MediaAssetTransfer) error {
	sourceRoot := w.Roots[transfer.SourceStore]
	objects := w.ObjectStores[transfer.SourceStore]
	if objects == nil {
		return fmt.Errorf("derived source object store %q is not configured", transfer.SourceStore)
	}
	if sourceRoot != "" {
		if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "deleting corrupt derived source"); err != nil {
			return err
		}
	}
	if err := objects.Delete(ctx, transfer.SourceRelPath); err != nil {
		return fmt.Errorf("deleting corrupt derived source object: %w", err)
	}
	if transfer.Kind == store.AssetKindEncoded && canHaveSubtitleSidecar(transfer.SourceRelPath) {
		sidecar, err := mediapath.SubtitleSibling(transfer.SourceRelPath)
		if err != nil {
			return fmt.Errorf("building corrupt derived source subtitle key: %w", err)
		}
		if err := objects.Delete(ctx, sidecar); err != nil {
			return fmt.Errorf("deleting corrupt derived source subtitle object: %w", err)
		}
	}
	if sourceRoot != "" {
		if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "marking corrupt derived source deleted"); err != nil {
			return err
		}
	}
	if _, err := w.Storage.MarkMediaAssetCopyDeleted(ctx, store.MediaAssetCopyDeleteCandidate{
		CopyID: transfer.SourceCopyID, MediaAssetID: transfer.MediaAssetID, Store: transfer.SourceStore,
	}); err != nil {
		return fmt.Errorf("marking corrupt derived source deleted: %w", err)
	}
	return nil
}

func sourceMatchesTransferMetadata(transfer store.MediaAssetTransfer, sourceSize int64, sourceSHA []byte) bool {
	return sourceSize == transfer.SizeBytes && (len(transfer.SHA256) == 0 || bytes.Equal(sourceSHA, transfer.SHA256))
}

func (w *StorageTransferWorker) recordTransferFailure(ctx context.Context, transfer store.MediaAssetTransfer, log *slog.Logger, cause error) error {
	if ctx.Err() != nil {
		return nil //nolint:nilerr // Shutdown cancellation must not count as a transfer failure.
	}
	if err := w.Storage.RecordMediaAssetTransferFailure(ctx, transfer.CopyID, cause.Error()); err != nil {
		return fmt.Errorf("recording transfer failure after %w: %w", cause, err)
	}
	log.Warn("storage transfer: copy attempt failed", "err", cause)
	return nil
}

type transferPathLock struct {
	store   string
	root    string
	relPath string
}

func lockTransferPaths(ctx context.Context, paths []transferPathLock) ([]*mediaRelPathFileLock, error) {
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].store != paths[j].store {
			return paths[i].store < paths[j].store
		}
		return paths[i].relPath < paths[j].relPath
	})
	var locks []*mediaRelPathFileLock
	for _, path := range paths {
		lock, err := lockMediaRelPathFile(ctx, path.root, path.relPath)
		if err != nil {
			closeTransferLocks(locks)
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func closeTransferLocks(locks []*mediaRelPathFileLock) {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = locks[i].Close()
	}
}

func hashTransferFile(ctx context.Context, path string) ([]byte, int64, error) {
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
	count, err := io.Copy(hasher, &storageTransferContextReader{ctx: ctx, reader: f})
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

func (w *StorageTransferWorker) transferSourceReader(ctx context.Context, sourceStore string, source io.Reader) (io.Reader, error) {
	bandwidth, err := w.Config.TransferBandwidthBytesPerSecond(sourceStore)
	if err != nil {
		return nil, fmt.Errorf("parsing source bandwidth limit: %w", err)
	}
	reader := io.Reader(&storageTransferContextReader{ctx: ctx, reader: source})
	if bandwidth > 0 {
		reader = &transferRateReader{ctx: ctx, reader: reader, bytesPerSecond: bandwidth, started: time.Now()}
	}
	return reader, nil
}

// hashTransferSource は転送元の全体を読んで SHA-256 とサイズを返す。ローカルはサイズが分かるので、読んでいる途中で変わったら失敗にする。
func (w *StorageTransferWorker) hashTransferSource(ctx context.Context, src transferSource, sourceStore string) ([]byte, int64, error) {
	knownSize, err := src.precheck()
	if err != nil {
		return nil, 0, err
	}
	reader, err := src.open(ctx, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("opening transfer source: %w", err)
	}
	defer func() { _ = reader.Close() }()
	limited, err := w.transferSourceReader(ctx, sourceStore, reader)
	if err != nil {
		return nil, 0, err
	}
	hasher := sha256.New()
	count, copyErr := io.Copy(hasher, limited)
	if copyErr != nil {
		return nil, count, fmt.Errorf("hashing transfer source: %w", copyErr)
	}
	if knownSize >= 0 && count != knownSize {
		return nil, count, fmt.Errorf("transfer source changed while hashing: read=%d size=%d", count, knownSize)
	}
	return hasher.Sum(nil), count, nil
}

type storageTransferContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *storageTransferContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type transferRateReader struct {
	ctx            context.Context
	reader         io.Reader
	bytesPerSecond int64
	started        time.Time
	bytesRead      int64
}

func (r *transferRateReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n == 0 || r.bytesPerSecond <= 0 {
		return n, err
	}
	r.bytesRead += int64(n)
	wantElapsed := time.Duration(float64(r.bytesRead) / float64(r.bytesPerSecond) * float64(time.Second))
	if wait := time.Until(r.started.Add(wantElapsed)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		case <-timer.C:
		}
	}
	return n, err
}

var _ io.Reader = (*storageTransferContextReader)(nil)
var _ io.Reader = (*transferRateReader)(nil)
