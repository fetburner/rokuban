// object_delete.go は、派生物の置き場が S3 のときの削除 reconcile を担当する。
//
// FS の置き場と同じ削除の述語（ごみ箱・外した版・旧世代）を、S3 の置き場に束ねた
// store.MediaDelete で引く。旧世代は公開の tx では触らず、overlap（storage.transfer.overlap）の後に
// ここが active → deleting → key の削除 → deleted の順で消す。署名付き URL で旧世代を再生中の視聴者が
// overlap の間は途中で 404 にならないようにするためである。
//
// 削除ブレーカーの範囲（scope）は派生物の置き場の名前で、FS の置き場のブレーカーとは件数を合算しない。
// 公開されないまま猶予を過ぎた pending の回収も、新しい削除の判断としてこのブレーカーの件数に数える。

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

// objectDeleteCandidates は S3 の置き場の 1 パス分の新規削除対象。
// DB に無い key は報告だけで削除しないので含めない。
type objectDeleteCandidates struct {
	trash          []store.MediaDeleteAsset
	removedEncoded []store.MediaDeleteAsset
	oldGeneration  []store.MediaDeleteAsset
	agedPending    []store.MediaDeleteAsset
}

func (c objectDeleteCandidates) total() int {
	return len(c.trash) + len(c.removedEncoded) + len(c.oldGeneration) + len(c.agedPending)
}

// objectStorePass は S3 の置き場 1 つ分の削除 reconcile を 1 パス実行する。
// DerivedObjects を設定していないときは何もしない。
func (w *DeleteReconcileWorker) objectStorePass(
	ctx context.Context, trashCutoff, overlapCutoff time.Time,
	orphanMTimeGrace, orphanAge time.Duration, maxPerPass int,
) error {
	if w.DerivedObjects == nil && w.DerivedMediaDelete == nil {
		return nil
	}
	if w.DerivedObjects == nil || w.DerivedMediaDelete == nil || w.DerivedPendingReclaim == nil || w.DerivedStorage == nil ||
		w.DerivedObjectStoreName == "" {
		return errors.New("derived object store, its copy stores, and its store name must be configured together")
	}
	scope := w.DerivedObjectStoreName
	tripped, err := w.observeDeleteReconcile(ctx, w.Breakers, scope)
	if err != nil {
		return err
	}
	if err := w.reconcileObjectOrphans(ctx, orphanMTimeGrace, orphanAge); err != nil {
		return err
	}
	placementRules, placementDerived, _, err := w.placementPolicy()
	if err != nil {
		return err
	}
	if err := w.restoreUnqualifiedObjectCopies(ctx, trashCutoff, overlapCutoff, placementRules, placementDerived); err != nil {
		return err
	}
	// 前パスで deleting のまま止まった行の再開は、既に決めた削除の再実行なのでブレーカーの対象外にする。
	pending, err := w.DerivedMediaDelete.ListPendingDeleteAssets(ctx, trashCutoff, overlapCutoff, placementRules, placementDerived, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing pending object copy deletes: %w", err)
	}
	for _, copy := range pending {
		w.deleteObjectCopy(ctx, copy, "pending")
	}

	var c objectDeleteCandidates
	if c.trash, err = w.DerivedMediaDelete.ListTrashAssetsToDelete(ctx, trashCutoff, deleteReconcileRowLimit); err != nil {
		return fmt.Errorf("listing trash object copies past retention: %w", err)
	}
	if c.removedEncoded, err = w.DerivedMediaDelete.ListRemovedEncodedAssetsToDelete(ctx, deleteReconcileRowLimit); err != nil {
		return fmt.Errorf("listing removed encoded object copies: %w", err)
	}
	if c.oldGeneration, err = w.DerivedMediaDelete.ListOldGenerationMediaAssetCopiesToDelete(ctx, overlapCutoff, deleteReconcileRowLimit); err != nil {
		return fmt.Errorf("listing old-generation object copies: %w", err)
	}
	pendingBefore := time.Now().Add(-orphanAge)
	if c.agedPending, err = w.DerivedPendingReclaim.ListAgedPendingMediaAssetCopies(ctx, pendingBefore, deleteReconcileRowLimit); err != nil {
		return fmt.Errorf("listing aged pending object copies: %w", err)
	}
	stop, err := w.applyDeleteCircuitBreaker(ctx, w.Breakers, scope, tripped, c.total(), maxPerPass)
	if err != nil {
		return err
	}
	if stop {
		return nil
	}
	for _, copy := range c.trash {
		w.deleteObjectCopy(ctx, copy, "trash")
	}
	for _, copy := range c.removedEncoded {
		w.deleteObjectCopy(ctx, copy, "removed_encoded")
	}
	for _, copy := range c.oldGeneration {
		w.deleteObjectCopy(ctx, copy, "old_generation")
	}
	for _, copy := range c.agedPending {
		w.reclaimPendingObjectCopy(ctx, copy, pendingBefore)
	}
	return nil
}

// reconcileObjectOrphans は、DB のコピー行に無い key を孤児候補として記録し、報告だけして残す。
// DB を復元した直後に、唯一の実体を消さないためである。
func (w *DeleteReconcileWorker) reconcileObjectOrphans(ctx context.Context, orphanMTimeGrace, orphanAge time.Duration) error {
	objects, err := w.DerivedObjects.List(ctx, "")
	if err != nil {
		metrics.RecordObjectStoreError("list", err)
		return fmt.Errorf("listing derived object store: %w", err)
	}
	known, err := w.DerivedStorage.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return fmt.Errorf("listing known derived object keys: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known)*2)
	for _, key := range known {
		knownSet[key] = struct{}{}
		if canHaveSubtitleSidecar(key) {
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
			metrics.RecordObjectStoreError("head", headErr)
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

// restoreUnqualifiedObjectCopies は削除条件から外れた deleting のコピーを、key の現存を確かめてから
// active に戻すか deleted を確定する。FS の resolveUnqualifiedDeletingAsset と同じ判断で、stat が Head になる。
func (w *DeleteReconcileWorker) restoreUnqualifiedObjectCopies(ctx context.Context, graceCutoff, overlapCutoff time.Time, placementRules []byte, placementDerived string) error {
	rows, err := w.DerivedMediaDelete.ListUnqualifiedDeletingAssets(ctx, graceCutoff, overlapCutoff, placementRules, placementDerived, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing unqualified deleting object copies: %w", err)
	}
	for _, row := range rows {
		_, headErr := w.DerivedObjects.Head(ctx, row.RelPath)
		if errors.Is(headErr, objstore.ErrNotFound) {
			if err := w.DerivedMediaDelete.MarkDeleted(ctx, row.ID); err != nil {
				return fmt.Errorf("finalizing absent deleting object copy %d: %w", row.ID, err)
			}
			continue
		}
		if headErr != nil {
			metrics.RecordObjectStoreError("head", headErr)
			return fmt.Errorf("checking deleting object %q before revert: %w", row.RelPath, headErr)
		}
		if _, err := w.DerivedMediaDelete.RevertToActive(ctx, row.ID, graceCutoff, overlapCutoff, placementRules, placementDerived); err != nil {
			return fmt.Errorf("reverting deleting object copy %d: %w", row.ID, err)
		}
	}
	return nil
}

// deleteObjectCopy は 1 コピーを active → deleting → key の削除 → deleted の順で消す。
// 各段階の失敗はログに残すだけで、行を deleting のまま次パスへ持ち越す。
// FS の deleteMediaAsset と違い、S3 の key には rel_path の flock も置き場の目印もない。
func (w *DeleteReconcileWorker) deleteObjectCopy(ctx context.Context, t store.MediaDeleteAsset, source string) {
	log := slog.With("media_asset_id", t.MediaAssetID, "media_asset_copy_id", t.ID, "rel_path", t.RelPath,
		"source", source, "store", w.DerivedObjectStoreName)
	if source != "pending" {
		marked, err := w.DerivedMediaDelete.MarkDeleting(ctx, t.ID, "active")
		if err != nil {
			log.Error("delete_reconcile: marking object copy deleting", "err", err)
			return
		}
		if !marked {
			return
		}
	}
	if err := w.deleteObjectKey(ctx, t.RelPath, t.Kind); err != nil {
		log.Error("delete_reconcile: deleting object", "err", err)
		return
	}
	if err := w.DerivedMediaDelete.MarkDeleted(ctx, t.ID); err != nil {
		log.Error("delete_reconcile: marking object copy deleted", "err", err)
		return
	}
	metrics.DeleteReconcileDeleted.WithLabelValues(source).Inc()
	metrics.DeleteReconcileBytes.WithLabelValues(source).Add(float64(t.SizeBytes))
	log.Info("delete_reconcile: deleted object copy")
}

// reclaimPendingObjectCopy は公開されないまま猶予を過ぎた pending の key を消す。
// pending は一度も現行のコピーになっていないので deleting を経由しない（deleting のまま残ると、
// 削除条件に当たらない行として active へ戻されてしまう）。行をロックしたまま消すので、
// 公開の tx が同じ行を active にする前後で、公開済みの key は消えない。
func (w *DeleteReconcileWorker) reclaimPendingObjectCopy(ctx context.Context, t store.MediaDeleteAsset, before time.Time) {
	log := slog.With("media_asset_id", t.MediaAssetID, "media_asset_copy_id", t.ID, "rel_path", t.RelPath,
		"source", "pending_reclaim", "store", w.DerivedObjectStoreName)
	reclaimed, err := w.DerivedPendingReclaim.ReclaimPendingMediaAssetCopy(ctx, t.ID, before, func() error {
		return w.deleteObjectKey(ctx, t.RelPath, t.Kind)
	})
	if err != nil {
		log.Error("delete_reconcile: reclaiming pending object copy", "err", err)
		return
	}
	if reclaimed {
		metrics.DeleteReconcileDeleted.WithLabelValues("pending_reclaim").Inc()
		log.Info("delete_reconcile: reclaimed pending object copy")
	}
}

// deleteObjectKey は key と、encoded に隣接する字幕の sidecar を消す。object の削除は冪等である。
func (w *DeleteReconcileWorker) deleteObjectKey(ctx context.Context, key, kind string) error {
	if err := w.DerivedObjects.Delete(ctx, key); err != nil {
		metrics.RecordObjectStoreError("delete", err)
		return fmt.Errorf("deleting derived object %q: %w", key, err)
	}
	if kind == store.AssetKindEncoded && canHaveSubtitleSidecar(key) {
		sidecar, err := mediapath.SubtitleSibling(key)
		if err != nil {
			return fmt.Errorf("building subtitle object key for %q: %w", key, err)
		}
		if err := w.DerivedObjects.Delete(ctx, sidecar); err != nil {
			metrics.RecordObjectStoreError("delete", err)
			return fmt.Errorf("deleting subtitle object %q: %w", sidecar, err)
		}
	}
	return nil
}

// canHaveSubtitleSidecar は、key の拡張子が字幕の sidecar（同名の .vtt）を持ちうる動画か返す。
// encode が字幕を作るのは .mp4 と .mkv の出力だけである。
func canHaveSubtitleSidecar(key string) bool {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".mp4", ".mkv":
		return true
	default:
		return false
	}
}
