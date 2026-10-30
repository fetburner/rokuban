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
	Storage store.StorageTransfer
	Config  config.StorageConfig
	Roots   map[string]string
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
	return w.copyAsset(ctx, args, rules, fsStores, targetRoot)
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

func (w *StorageTransferWorker) copyAsset(ctx context.Context, args jobs.StorageTransferArgs, rules []byte, fsStores []string, targetRoot string) (retErr error) {
	transfer, ok, err := w.Storage.PrepareMediaAssetTransfer(
		ctx, args.MediaAssetID, args.Store, rules, fsStores, w.Config.Derived, storageTransferAttemptLimit,
	)
	if err != nil || !ok {
		return err
	}
	log := slog.With("media_asset_id", transfer.MediaAssetID, "source_store", transfer.SourceStore, "target_store", args.Store)
	sourceRoot := w.Roots[transfer.SourceStore]
	if sourceRoot == "" {
		return jobq.Snooze(storageTransferLockSnooze)
	}
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
	if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "reading storage transfer source"); err != nil {
		return err
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
	paths = append(paths, transferPathLock{store: transfer.SourceStore, root: sourceRoot, relPath: transfer.SourceRelPath})
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
	if err := requireMediaStoreMarker(sourceRoot, transfer.SourceStore, "reading storage transfer source"); err != nil {
		return err
	}

	if handled, err := w.adoptExistingDestination(ctx, transfer, args, rules, targetPath, sourcePath, log); handled {
		return err
	}
	if err := requireMediaStoreMarker(targetRoot, args.Store, "creating transfer destination directory"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("creating transfer destination directory: %w", err))
	}
	tempPath := filepath.Join(filepath.Dir(targetPath), fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, transfer.MediaAssetID, transfer.CopyID))
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
	if fsink.SameFile(tempPath, targetPath) {
		digest, size, hashErr := hashTransferFile(ctx, targetPath)
		if hashErr != nil {
			return w.recordTransferFailure(ctx, transfer, log, hashErr)
		}
		if size != transfer.SizeBytes || (len(transfer.SHA256) > 0 && !bytes.Equal(digest, transfer.SHA256)) {
			return w.handleTransferMismatch(ctx, transfer, log, sourcePath, tempPath, "published transfer temp does not match the logical asset")
		}
		return w.completeTransfer(ctx, transfer, args, rules, digest, sourcePath, targetPath, tempPath, false, log)
	}
	digest, size, paused, copyErr := w.copyToTransferTemp(ctx, sourcePath, tempPath, tempLock, transfer.SourceStore, args.Store, transfer.SizeBytes, len(transfer.SHA256) > 0)
	if paused {
		return nil
	}
	if errors.Is(copyErr, errTransferSourceMismatch) {
		return w.handleTransferMismatch(ctx, transfer, log, sourcePath, tempPath, "transfer temp prefix differs from source")
	}
	if copyErr != nil {
		return w.recordTransferFailure(ctx, transfer, log, copyErr)
	}
	if size != transfer.SizeBytes {
		return w.handleTransferMismatch(ctx, transfer, log, sourcePath, tempPath, fmt.Sprintf("source size %d differs from logical asset size %d", size, transfer.SizeBytes))
	}
	if len(transfer.SHA256) > 0 && !bytes.Equal(digest, transfer.SHA256) {
		return w.handleTransferMismatch(ctx, transfer, log, sourcePath, tempPath, "transfer SHA-256 differs from logical asset")
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
	return w.completeTransfer(ctx, transfer, args, rules, digest, sourcePath, targetPath, tempPath, true, log)
}

func (w *StorageTransferWorker) adoptExistingDestination(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules []byte, targetPath, sourcePath string, log *slog.Logger) (bool, error) {
	_, statErr := os.Lstat(targetPath)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if statErr != nil {
		return true, w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("stating transfer destination: %w", statErr))
	}
	destinationSHA, size, err := hashTransferFile(ctx, targetPath)
	if err != nil {
		return true, w.recordTransferFailure(ctx, transfer, log, err)
	}
	if size == transfer.SizeBytes && len(transfer.SHA256) > 0 && bytes.Equal(destinationSHA, transfer.SHA256) {
		return true, w.completeTransfer(ctx, transfer, args, rules, destinationSHA, sourcePath, targetPath, "", false, log)
	}
	if size == transfer.SizeBytes && len(transfer.SHA256) == 0 {
		sourceSHA, sourceSize, sourceErr := w.hashTransferSource(ctx, sourcePath, transfer.SourceStore)
		if sourceErr != nil {
			return true, w.recordTransferFailure(ctx, transfer, log, sourceErr)
		}
		if sourceSize != transfer.SizeBytes {
			return true, w.handleTransferMismatch(ctx, transfer, log, sourcePath, "", "source size differs from logical asset")
		}
		if bytes.Equal(destinationSHA, sourceSHA) {
			return true, w.completeTransfer(ctx, transfer, args, rules, sourceSHA, sourcePath, targetPath, "", false, log)
		}
	}
	return true, w.handleTransferMismatch(ctx, transfer, log, sourcePath, "", fmt.Sprintf("destination already exists with different size or SHA-256 (size=%d, want=%d)", size, transfer.SizeBytes))
}

func (w *StorageTransferWorker) copyToTransferTemp(ctx context.Context, sourcePath, tempPath string, tempLock *os.File, sourceStore, targetStore string, expectedSize int64, sourceSHAKnown bool) ([]byte, int64, bool, error) {
	info, err := validateTransferSource(sourcePath, expectedSize)
	if err != nil {
		if errors.Is(err, errTransferSourceMismatch) {
			return nil, info.Size(), false, err
		}
		return nil, 0, false, err
	}
	if err := resetOversizedTransferTemp(tempPath, tempLock, expectedSize); err != nil {
		return nil, 0, false, err
	}
	paused, err := verifyCheckpointedTransferTemp(ctx, tempPath, tempLock)
	if paused || err != nil {
		return nil, info.Size(), paused, err
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
	source, err := os.Open(sourcePath)
	if err != nil {
		return nil, offset, false, fmt.Errorf("opening transfer source: %w", err)
	}
	defer func() { _ = source.Close() }()
	sourceReader, err := w.transferSourceReader(ctx, sourceStore, source)
	if err != nil {
		return nil, offset, false, err
	}
	paused, err = verifyTransferSourcePrefix(ctx, source, sourceReader, offset, sourceSHAKnown, hasher)
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

func validateTransferSource(sourcePath string, expectedSize int64) (os.FileInfo, error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("stating transfer source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("transfer source is not a regular file")
	}
	if info.Size() != expectedSize {
		return info, errTransferSourceMismatch
	}
	return info, nil
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

func verifyTransferSourcePrefix(ctx context.Context, source *os.File, sourceReader io.Reader, offset int64, sourceSHAKnown bool, tempHasher hash.Hash) (bool, error) {
	if offset == 0 {
		return false, nil
	}
	if sourceSHAKnown {
		if _, err := source.Seek(offset, io.SeekStart); err != nil {
			return false, fmt.Errorf("seeking transfer source to checkpoint offset %d: %w", offset, err)
		}
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

func (w *StorageTransferWorker) completeTransfer(ctx context.Context, transfer store.MediaAssetTransfer, args jobs.StorageTransferArgs, rules, digest []byte, sourcePath, targetPath, tempPath string, createdCanonical bool, log *slog.Logger) error {
	if err := requireMediaStoreMarker(w.Roots[args.Store], args.Store, "committing transferred media asset"); err != nil {
		return err
	}
	activated, rejection, err := w.Storage.CompleteMediaAssetTransfer(ctx, transfer, args.Store, w.Config.Derived, rules, digest)
	if err != nil {
		return err
	}
	if !activated {
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
			return w.handleTransferMismatch(ctx, transfer, log, sourcePath, tempPath, "completion rejected a stale SHA-256")
		}
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.CopyID); err != nil {
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

func (w *StorageTransferWorker) handleTransferMismatch(ctx context.Context, transfer store.MediaAssetTransfer, log *slog.Logger, sourcePath, tempPath, reason string) error {
	sourceSHA, sourceSize, err := w.hashTransferSource(ctx, sourcePath, transfer.SourceStore)
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

	corrupt, err := w.Storage.MarkMediaAssetCopyCorrupt(ctx, transfer.MediaAssetID, transfer.SourceCopyID, transfer.SourceStore)
	if err != nil {
		return err
	}
	if tempPath != "" {
		if err := fsink.RemoveTemp(tempPath); err != nil {
			log.Warn("storage transfer: removing mismatched temp failed", "path", tempPath, "err", err)
		}
	}
	if corrupt {
		if err := w.Storage.DiscardPendingMediaAssetCopy(ctx, transfer.CopyID); err != nil {
			return err
		}
		log.Warn("storage transfer: marked corrupt derived source for recreation", "reason", reason, "sha256", fmt.Sprintf("%x", sourceSHA))
		return nil
	}
	log.Error("storage transfer: derived source is inconsistent but no present original copy can rebuild it",
		"reason", reason, "sha256", fmt.Sprintf("%x", sourceSHA))
	return w.recordTransferFailure(ctx, transfer, log, fmt.Errorf("derived source mismatch without a present original: %s", reason))
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

func (w *StorageTransferWorker) hashTransferSource(ctx context.Context, path, sourceStore string) ([]byte, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("stating transfer source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("transfer source is not a regular file")
	}
	source, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening transfer source: %w", err)
	}
	reader, readerErr := w.transferSourceReader(ctx, sourceStore, source)
	if readerErr != nil {
		_ = source.Close()
		return nil, 0, readerErr
	}
	hasher := sha256.New()
	count, copyErr := io.Copy(hasher, reader)
	closeErr := source.Close()
	if copyErr != nil {
		return nil, count, fmt.Errorf("hashing transfer source: %w", copyErr)
	}
	if closeErr != nil {
		return nil, count, fmt.Errorf("closing transfer source after hashing: %w", closeErr)
	}
	if count != info.Size() {
		return nil, count, fmt.Errorf("transfer source changed while hashing: read=%d size=%d", count, info.Size())
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
