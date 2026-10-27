package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	"github.com/fetburner/rokuban/internal/store"
)

const (
	storageTransferAttemptLimit int32 = 10
	storageTransferRowLimit     int32 = 1000
)

var errTransferSourceMismatch = errors.New("source copy does not match the logical media asset")

// StorageTransferReconcileWorker enqueues transfer and stale-copy jobs from current policy state.
type StorageTransferReconcileWorker struct {
	Enqueuer jobq.Enqueuer
	Storage  store.StorageTransfer
	Config   config.StorageConfig
	RowLimit int32
}

// Work enqueues desired copies and stale-copy deletions after its read queries have completed.
func (w *StorageTransferReconcileWorker) Work(ctx context.Context, _ jobs.StorageTransferReconcileArgs) error {
	if w.Enqueuer == nil {
		return fmt.Errorf("storage transfer reconcile: job enqueuer is nil")
	}
	if w.Storage == nil {
		return fmt.Errorf("storage transfer reconcile: storage contract is nil")
	}
	fsStores := w.Config.FSStoreNames()
	if len(fsStores) < 2 {
		metrics.StorageTransferWaitingAssets.Set(0)
		metrics.StorageTransferWaitingBytes.Set(0)
		return nil
	}
	rules, err := w.Config.OriginalRulesJSON()
	if err != nil {
		return fmt.Errorf("encoding storage original rules: %w", err)
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
	transfers, err := w.Storage.ListMediaAssetTransferCandidates(ctx, rules, fsStores, w.Config.Derived, storageTransferAttemptLimit, rowLimit)
	if err != nil {
		return fmt.Errorf("listing storage transfer candidates: %w", err)
	}
	deletions, err := w.Storage.ListMediaAssetCopiesToDelete(ctx, rules, w.Config.Derived, w.Config.Transfer.Overlap.Microseconds(), rowLimit)
	if err != nil {
		return fmt.Errorf("listing stale storage copies: %w", err)
	}
	for _, candidate := range transfers {
		if err := enqueueJob(ctx, w.Enqueuer, jobs.StorageTransferArgs{
			Action: jobs.StorageTransferActionCopy, MediaAssetID: candidate.MediaAssetID,
			Store: candidate.Store, PolicyFingerprint: fingerprint,
		}); err != nil {
			slog.Error("storage transfer reconcile: failed to enqueue copy", "media_asset_id", candidate.MediaAssetID, "store", candidate.Store, "err", err)
		}
	}
	for _, candidate := range deletions {
		if err := enqueueJob(ctx, w.Enqueuer, jobs.StorageTransferArgs{
			Action: jobs.StorageTransferActionDelete, MediaAssetID: candidate.MediaAssetID,
			CopyID: candidate.CopyID, Store: candidate.Store, PolicyFingerprint: fingerprint,
		}); err != nil {
			slog.Error("storage transfer reconcile: failed to enqueue stale-copy delete",
				"media_asset_id", candidate.MediaAssetID, "copy_id", candidate.CopyID, "store", candidate.Store, "err", err)
		}
	}
	return nil
}

// StorageTransferWorker copies one logical asset between mounted FS stores or removes one stale copy.
type StorageTransferWorker struct {
	Storage store.StorageTransfer
	Config  config.StorageConfig
	Roots   map[string]string
}

// Work verifies the policy token before touching a store and then runs one copy operation.
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
	rules, err := w.Config.OriginalRulesJSON()
	if err != nil {
		return fmt.Errorf("encoding storage original rules: %w", err)
	}
	switch args.Action {
	case jobs.StorageTransferActionCopy:
		return w.copyAsset(ctx, args, rules)
	case jobs.StorageTransferActionDelete:
		return w.deleteCopy(ctx, args, rules)
	default:
		return fmt.Errorf("storage transfer: unknown action %q: %w", args.Action, jobq.ErrDiscard)
	}
}

func (w *StorageTransferWorker) copyAsset(ctx context.Context, args jobs.StorageTransferArgs, rules []byte) error {
	fsStores := w.Config.FSStoreNames()
	transfer, ok, err := w.Storage.PrepareMediaAssetTransfer(
		ctx, args.MediaAssetID, args.Store, rules, fsStores, w.Config.Derived, storageTransferAttemptLimit,
	)
	if err != nil || !ok {
		return err
	}
	log := slog.With("media_asset_id", transfer.MediaAssetID, "source_store", transfer.SourceStore, "target_store", args.Store)
	targetRoot, ok := w.Roots[args.Store]
	if !ok || targetRoot == "" {
		return fmt.Errorf("storage transfer: target store %q is not mounted on this worker", args.Store)
	}
	sourceRoot := w.Roots[transfer.SourceStore]
	if err := requireMediaStoreMarker(targetRoot, args.Store, "starting storage transfer"); err != nil {
		return err
	}
	if sourceRoot != "" {
		if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "reading storage transfer source"); err != nil {
			return err
		}
	}
	targetPath, err := mediapath.Resolve(targetRoot, transfer.RelPath)
	if err != nil {
		return fmt.Errorf("resolving transfer destination: %w", err)
	}
	var sourcePath string
	if sourceRoot != "" {
		sourcePath, err = mediapath.Resolve(sourceRoot, transfer.SourceRelPath)
		if err != nil {
			return fmt.Errorf("resolving transfer source: %w", err)
		}
	}
	paths := []transferPathLock{{store: args.Store, root: targetRoot, relPath: transfer.RelPath}}
	if sourcePath != "" {
		paths = append(paths, transferPathLock{store: transfer.SourceStore, root: sourceRoot, relPath: transfer.SourceRelPath})
	}
	locks, err := lockTransferPaths(ctx, paths)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return w.recordTransferFailure(ctx, transfer, args.Store, log, err)
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

	if handled, err := w.adoptExistingDestination(ctx, transfer, args, rules, targetPath, sourcePath, log); handled {
		return err
	}
	if sourcePath == "" {
		return w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("source store %q is not mounted on this worker", transfer.SourceStore))
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "creating transfer destination directory"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("creating transfer destination directory: %w", err))
	}
	tempPath := filepath.Join(filepath.Dir(targetPath), fmt.Sprintf(".rokuban-transfer-%d-%d", transfer.MediaAssetID, transfer.CopyID))
	tempLock, created, err := fsink.LockTempFile(tempPath)
	if err != nil {
		if fsink.TempLockBusy(err) {
			return nil
		}
		return w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("locking transfer temp: %w", err))
	}
	defer func() { _ = tempLock.Close() }()
	if created {
		if err := os.Chmod(tempPath, 0o644); err != nil {
			return w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("setting transfer temp mode: %w", err))
		}
	}
	if fsink.SameFile(tempPath, targetPath) {
		digest, size, hashErr := hashTransferFile(ctx, targetPath)
		if hashErr != nil {
			return w.recordTransferFailure(ctx, transfer, args.Store, log, hashErr)
		}
		if size != transfer.SizeBytes || (len(transfer.SHA256) > 0 && !bytes.Equal(digest, transfer.SHA256)) {
			return w.sourceMismatch(ctx, transfer, args.Store, log, digest, tempPath, "published transfer temp does not match the logical asset")
		}
		return w.completeTransfer(ctx, transfer, args, rules, digest, targetPath, tempPath, false, log)
	}
	digest, size, paused, copyErr := w.copyToTransferTemp(ctx, sourcePath, tempPath, tempLock, transfer.SourceStore, args.Store, transfer.SizeBytes)
	if paused {
		return nil
	}
	if errors.Is(copyErr, errTransferSourceMismatch) {
		sourceSHA, _, hashErr := hashTransferFile(ctx, sourcePath)
		if hashErr != nil {
			return w.recordTransferFailure(ctx, transfer, args.Store, log, hashErr)
		}
		return w.sourceMismatch(ctx, transfer, args.Store, log, sourceSHA, tempPath, "source content or size differs from logical asset")
	}
	if copyErr != nil {
		return w.recordTransferFailure(ctx, transfer, args.Store, log, copyErr)
	}
	if size != transfer.SizeBytes {
		return w.sourceMismatch(ctx, transfer, args.Store, log, digest, tempPath, fmt.Sprintf("source size %d differs from logical asset size %d", size, transfer.SizeBytes))
	}
	if len(transfer.SHA256) > 0 && !bytes.Equal(digest, transfer.SHA256) {
		return w.sourceMismatch(ctx, transfer, args.Store, log, digest, tempPath, "source SHA-256 differs from the logical asset")
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "publishing storage transfer"); err != nil {
		return err
	}
	if err := fsink.PublishLink(tempPath, targetPath); err != nil {
		return w.recordTransferFailure(ctx, transfer, args.Store, log, err)
	}
	if err := fsink.SyncParentDir(targetPath); err != nil {
		return w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("syncing transfer destination directory: %w", err))
	}
	return w.completeTransfer(ctx, transfer, args, rules, digest, targetPath, tempPath, true, log)
}

func (w *StorageTransferWorker) adoptExistingDestination(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules []byte, targetPath, sourcePath string, log *slog.Logger) (bool, error) {
	_, statErr := os.Lstat(targetPath)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if statErr != nil {
		return true, w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("stating transfer destination: %w", statErr))
	}
	destinationSHA, size, err := hashTransferFile(ctx, targetPath)
	if err != nil {
		return true, w.recordTransferFailure(ctx, transfer, args.Store, log, err)
	}
	if size == transfer.SizeBytes && len(transfer.SHA256) > 0 && bytes.Equal(destinationSHA, transfer.SHA256) {
		return true, w.completeTransfer(ctx, transfer, args, rules, destinationSHA, targetPath, "", false, log)
	}
	if size == transfer.SizeBytes && sourcePath != "" && len(transfer.SHA256) == 0 {
		sourceSHA, sourceSize, sourceErr := hashTransferFile(ctx, sourcePath)
		if sourceErr != nil {
			return true, w.recordTransferFailure(ctx, transfer, args.Store, log, sourceErr)
		}
		if sourceSize != transfer.SizeBytes {
			return true, w.sourceMismatch(ctx, transfer, args.Store, log, sourceSHA, "", "source size differs from logical asset")
		}
		if bytes.Equal(destinationSHA, sourceSHA) {
			return true, w.completeTransfer(ctx, transfer, args, rules, sourceSHA, targetPath, "", false, log)
		}
	}
	return true, w.recordTransferFailure(ctx, transfer, args.Store, log, fmt.Errorf("destination already exists with different size or SHA-256 (size=%d, want=%d)", size, transfer.SizeBytes))
}

func (w *StorageTransferWorker) copyToTransferTemp(ctx context.Context, sourcePath, tempPath string, tempLock *os.File, sourceStore, targetStore string, expectedSize int64) ([]byte, int64, bool, error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return nil, 0, false, fmt.Errorf("stating transfer source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("transfer source is not a regular file")
	}
	if info.Size() != expectedSize {
		return nil, info.Size(), false, errTransferSourceMismatch
	}
	tempInfo, err := tempLock.Stat()
	if err != nil {
		return nil, 0, false, fmt.Errorf("stating transfer temp: %w", err)
	}
	if tempInfo.Size() > expectedSize {
		if err := tempLock.Truncate(0); err != nil {
			return nil, 0, false, fmt.Errorf("truncating oversized transfer temp: %w", err)
		}
		if err := fsink.RemoveCheckpoint(tempPath); err != nil {
			return nil, 0, false, fmt.Errorf("removing checkpoint for oversized transfer temp: %w", err)
		}
	}
	hasher := sha256.New()
	offset, replayComplete, err := fsink.ReplayTempFileWithCheckpoint(ctx, tempPath, tempLock, hasher, func() error {
		return requireMediaStoreMarker(w.Roots[targetStore], targetStore, "saving transfer checkpoint")
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, true, nil
		}
		return nil, 0, false, fmt.Errorf("replaying transfer temp: %w", err)
	}
	if !replayComplete || offset > expectedSize {
		return nil, offset, false, fmt.Errorf("invalid transfer checkpoint offset %d for source size %d", offset, expectedSize)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return nil, offset, false, fmt.Errorf("opening transfer source: %w", err)
	}
	defer func() { _ = source.Close() }()
	bandwidth, err := w.Config.TransferBandwidthBytesPerSecond(sourceStore)
	if err != nil {
		return nil, offset, false, fmt.Errorf("parsing source bandwidth limit: %w", err)
	}
	sourceReader := io.Reader(&storageTransferContextReader{ctx: ctx, reader: source})
	if bandwidth > 0 {
		sourceReader = &transferRateReader{ctx: ctx, reader: sourceReader, bytesPerSecond: bandwidth, started: time.Now()}
	}
	sourcePrefixHasher := sha256.New()
	verifiedPrefixBytes, prefixErr := io.CopyN(sourcePrefixHasher, sourceReader, offset)
	if prefixErr != nil {
		if ctx.Err() != nil {
			return nil, offset, true, nil
		}
		if errors.Is(prefixErr, io.EOF) || errors.Is(prefixErr, io.ErrUnexpectedEOF) {
			return nil, verifiedPrefixBytes, false, errTransferSourceMismatch
		}
		return nil, verifiedPrefixBytes, false, fmt.Errorf("hashing current transfer source prefix: %w", prefixErr)
	}
	if !bytes.Equal(sourcePrefixHasher.Sum(nil), hasher.Sum(nil)) {
		return nil, offset, false, errTransferSourceMismatch
	}
	writer, err := fsink.OpenFileFromLock(tempLock)
	if err != nil {
		return nil, offset, false, fmt.Errorf("duplicating transfer temp descriptor: %w", err)
	}
	reader := io.LimitReader(sourceReader, expectedSize-offset)
	count, copyErr := io.CopyBuffer(fsink.NewHashingWriter(writer, hasher), reader, make([]byte, 128*1024))
	closeErr := writer.Close()
	written := offset + count
	if copyErr != nil {
		if ctx.Err() != nil {
			if err := fsink.PersistCheckpoint(tempPath, tempLock, written, hasher, func() error {
				return requireMediaStoreMarker(w.Roots[targetStore], targetStore, "persisting transfer checkpoint")
			}); err != nil {
				return nil, written, false, fmt.Errorf("persisting interrupted transfer: %w", err)
			}
			return nil, written, true, nil
		}
		return nil, written, false, fmt.Errorf("copying transfer source: %w", copyErr)
	}
	if closeErr != nil {
		return nil, written, false, fmt.Errorf("closing transfer temp writer: %w", closeErr)
	}
	if err := tempLock.Sync(); err != nil {
		return nil, written, false, fmt.Errorf("syncing transfer temp: %w", err)
	}
	if written != expectedSize {
		return hasher.Sum(nil), written, false, nil
	}
	if err := requireMediaStoreMarker(w.Roots[targetStore], targetStore, "writing transfer checkpoint"); err != nil {
		return nil, written, false, err
	}
	if err := fsink.WriteCheckpoint(tempPath, written, hasher, nil); err != nil {
		return nil, written, false, fmt.Errorf("writing final transfer checkpoint: %w", err)
	}
	return hasher.Sum(nil), written, false, nil
}

func (w *StorageTransferWorker) completeTransfer(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules, digest []byte, targetPath, tempPath string, createdCanonical bool, log *slog.Logger) error {
	if err := requireMediaStoreMarker(w.Roots[args.Store], args.Store, "committing transferred media asset"); err != nil {
		return err
	}
	activated, err := w.Storage.CompleteMediaAssetTransfer(ctx, transfer, args.Store, w.Config.Derived, rules, digest)
	if err != nil {
		return err
	}
	if !activated {
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.MediaAssetID, args.Store); err != nil {
			return err
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

func (w *StorageTransferWorker) sourceMismatch(ctx context.Context, transfer store.MediaAssetTransfer, targetStore string, log *slog.Logger, digest []byte, tempPath, reason string) error {
	if transfer.Kind != "original" {
		if err := w.Storage.MarkMediaAssetCopyCorrupt(ctx, transfer.MediaAssetID, transfer.SourceCopyID, transfer.SourceStore); err != nil {
			return err
		}
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.MediaAssetID, targetStore); err != nil {
			return err
		}
		log.Warn("storage transfer: marked corrupt derived source for recreation", "reason", reason, "sha256", fmt.Sprintf("%x", digest))
	} else {
		log.Error("storage transfer: original source does not match its logical asset; preserving source", "reason", reason, "sha256", fmt.Sprintf("%x", digest))
		if err := w.recordTransferFailure(ctx, transfer, targetStore, log, fmt.Errorf("%w: %s", errTransferSourceMismatch, reason)); err != nil {
			return err
		}
	}
	if tempPath != "" {
		if err := fsink.RemoveTemp(tempPath); err != nil {
			log.Warn("storage transfer: removing mismatched temp failed", "path", tempPath, "err", err)
		}
	}
	return nil
}

func (w *StorageTransferWorker) recordTransferFailure(ctx context.Context, transfer store.MediaAssetTransfer, targetStore string, log *slog.Logger, cause error) error {
	if ctx.Err() != nil {
		return nil //nolint:nilerr // Shutdown cancellation must not count as a transfer failure.
	}
	if err := w.Storage.RecordMediaAssetTransferFailure(ctx, transfer.MediaAssetID, targetStore, cause.Error()); err != nil {
		return fmt.Errorf("recording transfer failure after %w: %w", cause, err)
	}
	log.Warn("storage transfer: copy attempt failed", "err", cause)
	return nil
}

func (w *StorageTransferWorker) deleteCopy(ctx context.Context, args jobs.StorageTransferArgs, rules []byte) error {
	if args.CopyID <= 0 {
		return fmt.Errorf("storage transfer: delete action has no copy ID: %w", jobq.ErrDiscard)
	}
	candidate, ok, err := w.Storage.GetMediaAssetCopyForDeletion(ctx, store.MediaAssetCopyDeleteCandidate{
		CopyID: args.CopyID, MediaAssetID: args.MediaAssetID, Store: args.Store,
	})
	if err != nil || !ok {
		return err
	}
	root, ok := w.Roots[args.Store]
	if !ok || root == "" {
		return fmt.Errorf("storage transfer: store %q is not mounted on this worker", args.Store)
	}
	if err := requireMediaStoreMarker(root, args.Store, "starting stale-copy deletion"); err != nil {
		return err
	}
	path, err := mediapath.Resolve(root, candidate.RelPath)
	if err != nil {
		return fmt.Errorf("resolving stale copy path: %w", err)
	}
	locks, err := lockTransferPaths(ctx, []transferPathLock{{store: args.Store, root: root, relPath: candidate.RelPath}})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("locking stale copy before delete: %w", err)
	}
	defer closeTransferLocks(locks)
	markedPath, marked, err := w.Storage.MarkMediaAssetCopyDeleting(ctx, candidate, rules, w.Config.Derived, w.Config.Transfer.Overlap.Microseconds())
	if err != nil || !marked {
		return err
	}
	if markedPath != candidate.RelPath {
		return nil
	}
	if err := requireMediaStoreMarker(root, args.Store, "unlinking stale media copy"); err != nil {
		return err
	}
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("removing stale media copy: %w", removeErr)
	}
	if err := fsink.SyncParentDir(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("syncing stale copy directory after unlink: %w", err)
	}
	if err := requireMediaStoreMarker(root, args.Store, "marking stale copy deleted"); err != nil {
		return err
	}
	deleted, err := w.Storage.MarkMediaAssetCopyDeleted(ctx, candidate)
	if err != nil {
		return err
	}
	if deleted {
		metrics.DeleteReconcileDeleted.WithLabelValues("placement").Inc()
		metrics.DeleteReconcileBytes.WithLabelValues("placement").Add(float64(candidate.SizeBytes))
	}
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
