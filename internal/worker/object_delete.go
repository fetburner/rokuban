package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
)

func (w *DeleteReconcileWorker) reconcileObjectStore(ctx context.Context, orphanMTimeGrace, orphanAge time.Duration) error {
	if w.DerivedObjects == nil && w.DerivedStorage == nil {
		return nil
	}
	if w.DerivedObjects == nil || w.DerivedStorage == nil {
		return errors.New("derived object store and observation store must be configured together")
	}
	objects, err := w.DerivedObjects.List(ctx, "")
	if err != nil {
		metrics.ObjectStoreErrors.WithLabelValues("list").Inc()
		return fmt.Errorf("listing derived object store: %w", err)
	}
	known, err := w.DerivedStorage.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return fmt.Errorf("listing known derived object keys: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known)*2)
	for _, key := range known {
		knownSet[key] = struct{}{}
		if isSubtitleAsset(key) {
			if sidecar, pathErr := mediapath.SubtitleSibling(key); pathErr == nil {
				knownSet[sidecar] = struct{}{}
			}
		}
	}
	objectsByKey := make(map[string]objstore.Object, len(objects))
	for _, object := range objects {
		objectsByKey[object.Key] = object
	}
	observed, err := w.DerivedStorage.ListAllOrphanFiles(ctx)
	if err != nil {
		return fmt.Errorf("listing observed derived object orphans: %w", err)
	}
	for _, old := range observed {
		if _, isKnown := knownSet[old.RelPath]; isKnown {
			if err := w.DerivedStorage.DeleteOrphanFile(ctx, old.RelPath); err != nil {
				return fmt.Errorf("clearing adopted object orphan %q: %w", old.RelPath, err)
			}
			continue
		}
		if _, listed := objectsByKey[old.RelPath]; listed {
			continue
		}
		if _, headErr := w.DerivedObjects.Head(ctx, old.RelPath); errors.Is(headErr, objstore.ErrNotFound) {
			if err := w.DerivedStorage.DeleteOrphanFile(ctx, old.RelPath); err != nil {
				return fmt.Errorf("clearing absent object orphan %q: %w", old.RelPath, err)
			}
		} else if headErr != nil {
			metrics.ObjectStoreErrors.WithLabelValues("head").Inc()
			return fmt.Errorf("checking observed object orphan %q: %w", old.RelPath, headErr)
		}
	}
	mtimeCutoff := time.Now().Add(-orphanMTimeGrace)
	for _, object := range objects {
		if _, ok := knownSet[object.Key]; ok || object.LastModified.After(mtimeCutoff) {
			continue
		}
		if err := w.DerivedStorage.UpsertOrphanFile(ctx, object.Key); err != nil {
			return fmt.Errorf("recording unknown object key %q: %w", object.Key, err)
		}
	}
	aged, err := w.DerivedStorage.ListAgedOrphanFiles(ctx, orphanAge)
	if err != nil {
		return fmt.Errorf("listing aged unknown object keys: %w", err)
	}
	metrics.ObjectStoreOrphansUnresolved.Set(float64(len(aged)))
	for i, key := range aged {
		if i == orphanFileLogBudget {
			slog.Warn("delete_reconcile: additional aged unknown object keys omitted", "omitted", len(aged)-i)
			break
		}
		slog.Warn("delete_reconcile: aged object store key has no media asset copy; retaining it", "store", w.DerivedObjectStoreName, "key", key)
	}
	return nil
}

func (w *DeleteReconcileWorker) restoreUnqualifiedObjectCopies(ctx context.Context, graceCutoff time.Time) error {
	if w.DerivedObjects == nil || w.DerivedMediaDelete == nil {
		return nil
	}
	if w.DerivedCopyMaintenance == nil {
		return errors.New("derived copy maintenance store is not configured")
	}
	rows, err := w.DerivedMediaDelete.ListUnqualifiedDeletingAssets(ctx, graceCutoff, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing unqualified deleting object assets: %w", err)
	}
	for _, row := range rows {
		_, headErr := w.DerivedObjects.Head(ctx, row.RelPath)
		if errors.Is(headErr, objstore.ErrNotFound) {
			if err := w.DerivedCopyMaintenance.MarkDeletingMediaAssetCopyDeleted(ctx, row.CopyID); err != nil {
				return fmt.Errorf("finalizing absent deleting object copy %d: %w", row.CopyID, err)
			}
			continue
		}
		if headErr != nil {
			metrics.ObjectStoreErrors.WithLabelValues("head").Inc()
			return fmt.Errorf("checking deleting object %q before revert: %w", row.RelPath, headErr)
		}
		if _, err := w.DerivedMediaDelete.RevertToActive(ctx, row.ID, graceCutoff); err != nil {
			return fmt.Errorf("reverting deleting object asset %d: %w", row.ID, err)
		}
	}
	return nil
}

func (w *DeleteReconcileWorker) reclaimPendingObjectCopies(ctx context.Context, before time.Time) error {
	if w.DerivedObjects == nil || w.DerivedCopyMaintenance == nil {
		return nil
	}
	rows, err := w.DerivedCopyMaintenance.ListAgedPendingMediaAssetCopies(ctx, before, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing aged pending object copies: %w", err)
	}
	for _, row := range rows {
		claimed, err := w.DerivedCopyMaintenance.ClaimAgedPendingMediaAssetCopy(ctx, row.ID, before)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		if err := w.deleteObjectKey(ctx, row.RelPath, row.Kind); err != nil {
			return err
		}
		if err := w.DerivedCopyMaintenance.MarkDeletingMediaAssetCopyDeleted(ctx, row.ID); err != nil {
			return fmt.Errorf("marking reclaimed pending object copy %d deleted: %w", row.ID, err)
		}
	}
	return nil
}

func (w *DeleteReconcileWorker) deleteDeletingObjectCopies(ctx context.Context) error {
	if w.DerivedObjects == nil || w.DerivedCopyMaintenance == nil {
		return nil
	}
	rows, err := w.DerivedCopyMaintenance.ListDeletingMediaAssetCopies(ctx, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing deleting object copies: %w", err)
	}
	for _, row := range rows {
		if err := w.deleteObjectKey(ctx, row.RelPath, row.Kind); err != nil {
			return err
		}
		if err := w.DerivedCopyMaintenance.MarkDeletingMediaAssetCopyDeleted(ctx, row.ID); err != nil {
			return fmt.Errorf("marking deleting object copy %d deleted: %w", row.ID, err)
		}
	}
	return nil
}

func (w *DeleteReconcileWorker) deleteObjectKey(ctx context.Context, key, kind string) error {
	if err := w.DerivedObjects.Delete(ctx, key); err != nil {
		metrics.ObjectStoreErrors.WithLabelValues("delete").Inc()
		return fmt.Errorf("deleting derived object %q: %w", key, err)
	}
	if kind == store.AssetKindEncoded && isSubtitleAsset(key) {
		sidecar, err := mediapath.SubtitleSibling(key)
		if err != nil {
			return fmt.Errorf("building subtitle object key for %q: %w", key, err)
		}
		if err := w.DerivedObjects.Delete(ctx, sidecar); err != nil {
			metrics.ObjectStoreErrors.WithLabelValues("delete").Inc()
			return fmt.Errorf("deleting subtitle object %q: %w", sidecar, err)
		}
	}
	return nil
}

func isSubtitleAsset(key string) bool {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".mp4", ".mkv":
		return true
	default:
		return false
	}
}
