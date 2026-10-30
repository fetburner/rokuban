// delete_execute.go は削除条件の再評価、ブレーカー、削除中行の再開と物理削除を担当する。
// 読む表は circuit_breakers、recordings、media_assets と名前付き削除述語の参照先。
// 書く表は circuit_breakers、media_assets、encoded_asset_removal_requests、orphan_files。
//
// 実行は until_encoded の述語を、実行の直前に再評価する
// （MediaDelete.MarkUntilEncodedDeleting）。flock を deleting 遷移より前に取るので、
// この遷移は選定へ移せない。
//
// orphan_files の書き手は 2 者いる。候補は storage_audit.go が書き、実行が unlink の後に
// 行を消す。実行が消すのは、ctx が取り消された後も削除の事実を記録するためである。
//
// ファイル操作は次の表のとおり。派生物の canonical key は objstore.Store で操作し、
// FS 固有の staging と永続化は fsstore に閉じる。ここに残るファイル操作は原本と scratch/temp。
//
//	操作      原本                         派生物                     ingest / transfer temp
//	解決      mediapath.Resolve            objstore key                mediapath.Resolve
//	flock     media root の rel_path lock  なし（key は再利用しない）   temp 自身の flock
//	stat      巻き戻し時の現存確認         objstore.Head               mtime・サイズ・inode
//	unlink    os.Remove                    objstore.Delete             os.Remove
//
// 字幕 sidecar も encoded と同じ object API で削除する。原本と temp の flock・inode 照合は
// ファイルシステム上に残る。

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
)

// observeDeleteReconcile は削除 reconcile のブレーカー状態を DB の真実から観測する。
//
// パスの先頭でブレーカーの発動状態を DB の真実に合わせ直す（ObserveState の
// コメント参照。プロセス再起動でゲージが失われるため）。
func (w *DeleteReconcileWorker) observeDeleteReconcile(ctx context.Context, breakers store.Breakers, scope string) (bool, error) {
	tripped, err := breaker.ObserveState(ctx, breakers, scope, breaker.DeleteReconcile)
	if err != nil {
		return false, fmt.Errorf("observing %s circuit breaker: %w", breaker.DeleteReconcile, err)
	}
	return tripped, nil
}

// restoreUnqualifiedDeletingAssets は削除条件から外れた deleting 行を再評価する。
//
// deleting のまま止まっていたが、その後の復元などで trash / until_encoded
// どちらの判定にも該当しなくなった行を active に戻す（issue #105）。
// ListUnqualifiedDeletingAssets は候補を挙げるだけで書き込まない —— 各行
// について resolveUnqualifiedDeletingAsset がファイルの現存を確認してから
// active に戻すか deleted を確定するかを選ぶ（ファイルが既に無いのに
// active に戻すと「active なのにファイルが無い行」を作ってしまう。これは
// 案 B [復元時に deleting を同期的に active へ戻す] を却下した理由と同じ
// 罠で、revert 経路自身がこの窓を作らないようにする）。
func (w *DeleteReconcileWorker) restoreUnqualifiedDeletingAssets(ctx context.Context, trashCutoff, overlapCutoff time.Time, placementRules []byte, placementDerived string) error {
	rows, err := w.MediaDelete.ListUnqualifiedDeletingAssets(ctx, trashCutoff, overlapCutoff, placementRules, placementDerived, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing unqualified deleting assets: %w", err)
	}
	for _, a := range rows {
		w.resolveUnqualifiedDeletingAsset(ctx, a, trashCutoff, overlapCutoff, placementRules, placementDerived)
	}
	return nil
}

// applyDeleteCircuitBreaker は新規削除の上限を超えたとき、または発動中に削除を止める。
func (w *DeleteReconcileWorker) applyDeleteCircuitBreaker(ctx context.Context, breakers store.Breakers, scope string, tripped bool, total, maxPerPass int) (bool, error) {
	if total > maxPerPass {
		if err := breaker.Trip(ctx, breakers, scope, breaker.DeleteReconcile, maxPerPass, breaker.Sample{Total: total}); err != nil {
			return false, fmt.Errorf("tripping circuit breaker: %w", err)
		}
		return true, nil
	}
	if tripped {
		if total > 0 {
			slog.Warn("delete_reconcile: circuit breaker latched — withholding new deletes until manually resumed",
				"breaker", breaker.DeleteReconcile, "pending_deletes", total)
		}
		return true, nil
	}
	return false, nil
}

// deleteCandidates はごみ箱・until_encoded・外した版・旧世代と公開前の孤児候補を物理削除する。
func (w *DeleteReconcileWorker) deleteCandidates(ctx context.Context, c deleteCandidateSet, orphanMTimeGrace time.Duration) {
	for _, a := range c.trash {
		w.deleteMediaAsset(ctx, a, "trash")
	}
	for _, a := range c.untilEncoded {
		w.deleteMediaAsset(ctx, a, "until_encoded")
	}
	for _, a := range c.removedEncoded {
		w.deleteMediaAsset(ctx, a, "removed_encoded")
	}
	for _, a := range c.oldGeneration {
		w.deleteMediaAsset(ctx, a, "old_generation")
	}
	for _, a := range c.placement {
		w.deletePlacementCopy(ctx, a, c.placementArm)
	}
	for _, candidate := range c.obsoletePendingTransfer {
		if ctx.Err() != nil {
			return
		}
		w.reclaimObsoletePendingTransferCopy(ctx, candidate)
	}
	for _, copy := range c.agedPendingDerived {
		if ctx.Err() != nil {
			return
		}
		w.reclaimPendingFilesystemDerivedCopy(ctx, copy, c.pendingBefore)
	}
	for _, relPath := range c.agedOrphans {
		// ctx 死亡後は残りを打ち切る（DeleteOrphanFile 1 件ごとに
		// deleteOrphanCleanupTimeout を待ち切って進むと、DB が詰まった状態では
		// パス全体の締切を超えうる）。残った孤児は次パスの
		// agedUnpublishedOrphans が拾い直す（レベルトリガー、不変条件 5）。
		if ctx.Err() != nil {
			return
		}
		w.deleteOrphanFile(relPath, orphanMTimeGrace)
	}
}

// reclaimPendingFilesystemDerivedCopy は、共有 FS に Put 済み・未公開の派生物 key を回収する。
// DB 行をロックしている間に object store API で削除し、mount marker も削除前後に照合する。
func (w *DeleteReconcileWorker) reclaimPendingFilesystemDerivedCopy(ctx context.Context, copy store.MediaDeleteAsset, before time.Time) {
	log := slog.With("media_asset_id", copy.MediaAssetID, "media_asset_copy_id", copy.ID,
		"store", w.MediaStoreName, "rel_path", copy.RelPath, "source", "pending_reclaim")
	if ctx.Err() != nil {
		return
	}
	reclaimed, err := w.MediaPendingReclaim.ReclaimPendingMediaAssetCopy(ctx, copy.ID, before, func() error {
		if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "reclaiming pending derived object"); err != nil {
			return err
		}
		if err := w.deleteObjectKey(ctx, copy.RelPath, copy.Kind); err != nil {
			return err
		}
		return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "marking pending derived object deleted")
	})
	if err != nil {
		log.Error("delete_reconcile: reclaiming pending filesystem derived copy", "err", err)
		return
	}
	if reclaimed {
		metrics.DeleteReconcileDeleted.WithLabelValues("pending_reclaim").Inc()
		log.Info("delete_reconcile: reclaimed pending filesystem derived copy")
	}
}

// deletePlacementCopy は desired の置き場から外れた active のコピーを deleting にし、unlink して deleted にする。
// 新規の候補だけを扱う。deleting のまま止まった行は ListPendingDeleteAssets が拾い直し、deleteMediaAsset が終わらせる。
func (w *DeleteReconcileWorker) deletePlacementCopy(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate, arm placementArm) {
	log := slog.With("media_asset_id", candidate.MediaAssetID, "media_asset_copy_id", candidate.CopyID,
		"store", candidate.Store, "rel_path", candidate.RelPath, "source", "placement")
	if ctx.Err() != nil {
		return
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "deleting stale placement copy"); err != nil {
		log.Warn("delete_reconcile: withholding placement deletion because the store marker did not match", "err", err)
		return
	}
	if candidate.Kind != store.AssetKindOriginal {
		if w.DerivedObjects == nil || candidate.Store != w.DerivedObjectStoreName {
			log.Error("delete_reconcile: derived placement object store is not configured", "derived_store", w.DerivedObjectStoreName)
			return
		}
		markedPath, marked, err := w.StorageTransfer.MarkMediaAssetCopyDeleting(ctx, candidate, arm.rules, arm.derived, arm.overlapCutoff)
		if err != nil {
			log.Error("delete_reconcile: rechecking derived placement copy before delete", "err", err)
			return
		}
		if !marked || markedPath != candidate.RelPath {
			return
		}
		beforeMediaAssetUnlink(candidate.RelPath)
		if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "deleting stale derived placement copy"); err != nil {
			log.Warn("delete_reconcile: withholding derived placement deletion because the store marker did not match", "err", err)
			return
		}
		if err := w.deleteObjectKey(ctx, candidate.RelPath, candidate.Kind); err != nil {
			log.Error("delete_reconcile: deleting stale derived placement object", "err", err)
			return
		}
		if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "marking stale derived placement copy deleted"); err != nil {
			log.Warn("delete_reconcile: withholding deleted state because the store marker did not match", "err", err)
			return
		}
		deleted, err := w.StorageTransfer.MarkMediaAssetCopyDeleted(ctx, candidate)
		if err != nil {
			log.Error("delete_reconcile: marking derived placement copy deleted", "err", err)
			return
		}
		if deleted {
			metrics.DeleteReconcileDeleted.WithLabelValues("placement").Inc()
			metrics.DeleteReconcileBytes.WithLabelValues("placement").Add(float64(candidate.SizeBytes))
			log.Info("delete_reconcile: deleted stale derived placement copy")
		}
		return
	}
	path, err := mediapath.Resolve(w.MediaDir, candidate.RelPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting placement rel_path outside the media directory", "err", err)
		return
	}
	fileLock, err := lockMediaRelPathFile(ctx, w.MediaDir, candidate.RelPath)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("delete_reconcile: acquiring filesystem lock before placement removal", "err", err)
		}
		return
	}
	if fileLock != nil {
		defer func() { _ = fileLock.Close() }()
	}
	markedPath, marked, err := w.StorageTransfer.MarkMediaAssetCopyDeleting(ctx, candidate, arm.rules, arm.derived, arm.overlapCutoff)
	if err != nil {
		log.Error("delete_reconcile: rechecking placement copy before unlink", "err", err)
		return
	}
	if !marked || markedPath != candidate.RelPath {
		return
	}
	beforeMediaAssetUnlink(candidate.RelPath)
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking stale placement copy"); err != nil {
		log.Warn("delete_reconcile: withholding placement unlink because the store marker did not match", "err", err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("delete_reconcile: removing stale placement copy", "err", err)
		return
	}
	if candidate.Kind == store.AssetKindEncoded && canHaveSubtitleSidecar(candidate.RelPath) {
		if err := removeEncodedSubtitleSidecar(w.MediaDir, w.MediaStoreName, candidate.RelPath, "unlinking stale placement subtitle sidecar"); err != nil {
			log.Error("delete_reconcile: removing stale placement subtitle sidecar", "err", err)
			return
		}
	}
	if err := fsink.SyncParentDir(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("delete_reconcile: syncing directory after placement unlink", "err", err)
		return
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "marking stale placement copy deleted"); err != nil {
		log.Warn("delete_reconcile: withholding deleted state because the store marker did not match", "err", err)
		return
	}
	deleted, err := w.StorageTransfer.MarkMediaAssetCopyDeleted(ctx, candidate)
	if err != nil {
		log.Error("delete_reconcile: marking placement copy deleted", "err", err)
		return
	}
	if deleted {
		metrics.DeleteReconcileDeleted.WithLabelValues("placement").Inc()
		metrics.DeleteReconcileBytes.WithLabelValues("placement").Add(float64(candidate.SizeBytes))
		log.Info("delete_reconcile: deleted stale placement copy")
	}
}

// reclaimObsoletePendingTransferCopy retires an unpublished transfer reservation after its
// current placement has changed. It takes the same path lock as transfer before locking DB rows.
func (w *DeleteReconcileWorker) reclaimObsoletePendingTransferCopy(ctx context.Context, candidate store.MediaAssetCopyDeleteCandidate) {
	log := slog.With("media_asset_id", candidate.MediaAssetID, "media_asset_copy_id", candidate.CopyID,
		"store", candidate.Store, "rel_path", candidate.RelPath, "source", "pending_transfer_reclaim")
	if ctx.Err() != nil {
		return
	}
	if w.StorageTransfer == nil || candidate.Store != w.MediaStoreName {
		log.Error("delete_reconcile: cannot reclaim pending transfer copy outside this worker's store")
		return
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "reclaiming obsolete pending transfer copy"); err != nil {
		log.Warn("delete_reconcile: withholding pending transfer reclaim because the store marker did not match", "err", err)
		return
	}
	if candidate.Kind != store.AssetKindOriginal {
		if w.DerivedObjects == nil || candidate.Store != w.DerivedObjectStoreName {
			log.Error("delete_reconcile: derived transfer object store is not configured", "derived_store", w.DerivedObjectStoreName)
			return
		}
		rules, err := w.StorageTransferPolicy.OriginalRulesJSON()
		if err != nil {
			log.Error("delete_reconcile: encoding current transfer policy for derived pending reclaim", "err", err)
			return
		}
		fsStores := w.StorageTransferPolicy.FSStoreNames()
		reclaimed, err := w.StorageTransfer.ReclaimObsoletePendingMediaAssetTransferCopy(
			ctx, candidate, rules, fsStores, w.StorageTransferPolicy.Derived,
			func() error {
				if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking obsolete pending derived transfer copy"); err != nil {
					return err
				}
			return w.deleteObjectKey(ctx, candidate.RelPath, candidate.Kind)
			},
		)
		if err != nil {
			log.Error("delete_reconcile: reclaiming obsolete pending derived transfer copy", "err", err)
			return
		}
		if reclaimed {
			metrics.DeleteReconcileDeleted.WithLabelValues("pending_transfer_reclaim").Inc()
			log.Info("delete_reconcile: reclaimed obsolete pending derived transfer copy")
		}
		return
	}
	path, err := mediapath.Resolve(w.MediaDir, candidate.RelPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting pending transfer rel_path outside the media directory", "err", err)
		return
	}
	fileLock, err := lockMediaRelPathFile(ctx, w.MediaDir, candidate.RelPath)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("delete_reconcile: acquiring filesystem lock before pending transfer reclaim", "err", err)
		}
		return
	}
	if fileLock != nil {
		defer func() { _ = fileLock.Close() }()
	}
	rules, err := w.StorageTransferPolicy.OriginalRulesJSON()
	if err != nil {
		log.Error("delete_reconcile: encoding current transfer policy for pending reclaim", "err", err)
		return
	}
	beforeMediaAssetUnlink(candidate.RelPath)
	fsStores := w.StorageTransferPolicy.FSStoreNames()
	tempPath := transferTempPath(path, candidate.MediaAssetID, candidate.CopyID)
	subtitleTempPath := transferSubtitleTempPath(path, candidate.MediaAssetID, candidate.CopyID)
	reclaimed, err := w.StorageTransfer.ReclaimObsoletePendingMediaAssetTransferCopy(
		ctx, candidate, rules, fsStores, w.StorageTransferPolicy.Derived,
		func() error {
			if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking obsolete pending transfer copy"); err != nil {
				return err
			}
			owned, err := pendingTransferDestinationOwnedByTemp(path, tempPath)
			if err != nil {
				return fmt.Errorf("checking pending transfer destination ownership: %w", err)
			}
			if owned && candidate.Kind == store.AssetKindEncoded && canHaveSubtitleSidecar(candidate.RelPath) {
				if err := removeEncodedSubtitleSidecar(w.MediaDir, w.MediaStoreName, candidate.RelPath, "unlinking pending transfer subtitle sidecar"); err != nil {
					return err
				}
			}
			if owned {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("removing pending transfer destination: %w", err)
				}
			} else if _, err := os.Lstat(path); err == nil {
				log.Warn("delete_reconcile: preserving pending transfer destination not owned by this transfer")
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stating pending transfer destination: %w", err)
			}
			if err := removeTransferTempUnderPathLock(tempPath); err != nil {
				return fmt.Errorf("removing pending transfer temp and checkpoint: %w", err)
			}
			if err := removeTransferTempUnderPathLock(subtitleTempPath); err != nil {
				return fmt.Errorf("removing pending transfer subtitle temp: %w", err)
			}
			if err := fsink.SyncParentDir(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("syncing pending transfer directory: %w", err)
			}
			if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "marking obsolete pending transfer copy deleted"); err != nil {
				return err
			}
			return nil
		},
	)
	if err != nil {
		log.Error("delete_reconcile: reclaiming obsolete pending transfer copy", "err", err)
		return
	}
	if reclaimed {
		metrics.DeleteReconcileDeleted.WithLabelValues("pending_transfer_reclaim").Inc()
		log.Info("delete_reconcile: reclaimed obsolete pending transfer copy")
	}
}

func pendingTransferDestinationOwnedByTemp(path, tempPath string) (bool, error) {
	destinationInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stating destination: %w", err)
	}
	if !destinationInfo.Mode().IsRegular() {
		return false, nil
	}
	tempInfo, err := os.Lstat(tempPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stating transfer temp: %w", err)
	}
	if !tempInfo.Mode().IsRegular() {
		return false, nil
	}
	return os.SameFile(destinationInfo, tempInfo), nil
}

func removeEncodedSubtitleSidecar(mediaDir, storeName, relPath, operation string) error {
	sidecarRelPath, err := mediapath.SubtitleSibling(relPath)
	if err != nil {
		return fmt.Errorf("building encoded subtitle sidecar path: %w", err)
	}
	sidecarPath, err := mediapath.Resolve(mediaDir, sidecarRelPath)
	if err != nil {
		return fmt.Errorf("resolving encoded subtitle sidecar path: %w", err)
	}
	if err := requireMediaStoreMarker(mediaDir, storeName, operation); err != nil {
		return err
	}
	info, err := os.Lstat(sidecarPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stating encoded subtitle sidecar: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("encoded subtitle sidecar is not a regular file: %s", sidecarPath)
	}
	if err := os.Remove(sidecarPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing encoded subtitle sidecar: %w", err)
	}
	if err := fsink.SyncParentDir(sidecarPath); err != nil {
		return fmt.Errorf("syncing directory after encoded subtitle sidecar removal: %w", err)
	}
	return nil
}

func removeTransferTempUnderPathLock(path string) error {
	tempLock, err := fsink.LockExistingTempFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("locking transfer temp before removal: %w", err)
	}
	if tempLock != nil {
		defer func() { _ = tempLock.Close() }()
	}
	return fsink.RemoveTemp(path)
}

// deleteMediaAsset は 1 コピーを冪等な 3 段階（active → deleting → unlink →
// deleted）で物理削除する。各段階のエラーはログに残すだけで呼び出し元には
// 伝播しない — 1 件の失敗でパス全体を止めると、後続の正常な削除まで巻き込む
// ため（record_sweep の processRecord と同じ設計判断）。deleting のまま
// 終わった行は次パスの MediaDelete.ListPendingDeleteAssets が拾い直す。
//
// 「録画そのものが消えたか」の判定は呼び出し元ではなく MarkPurgedRecordings
// が録画単位・パス末尾で行う（issue #135）ため、ここでは deleted まで到達
// したかどうかを戻さない。
func (w *DeleteReconcileWorker) deleteMediaAsset(ctx context.Context, t store.MediaDeleteAsset, source string) {
	log := slog.With("media_asset_id", t.MediaAssetID, "media_asset_copy_id", t.ID, "rel_path", t.RelPath, "source", source)
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "deleting media asset"); err != nil {
		log.Warn("delete_reconcile: withholding asset deletion because media store marker did not match", "err", err)
		return
	}
	if t.Kind != store.AssetKindOriginal {
		if w.DerivedObjects == nil {
			log.Error("delete_reconcile: derived object store is not configured")
			return
		}
		w.deleteObjectCopyWithStore(ctx, w.MediaDelete, t, source, w.MediaStoreName,
			func(operation string) error { return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, operation) })
		return
	}

	path, err := mediapath.Resolve(w.MediaDir, t.RelPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting rel_path outside the media directory", "err", err)
		return
	}

	// canonical の書き手と同じ filesystem lock を、DB の deleting 遷移より前に取る。
	// 通常削除が canonical を unlink している間に ingest / encode が同じ rel_path を
	// 公開すると、DB 行と実体の組が入れ替わる窓ができる。
	// lock は canonical の親ではなく media root に置くため、canonical の親が無くても
	// 排他できる。media root / lock directory にアクセスできない場合は安全側で何もしない。
	fileLock, lockErr := lockMediaRelPathFile(ctx, w.MediaDir, t.RelPath)
	if lockErr != nil {
		log.Error("delete_reconcile: acquiring filesystem lock before asset removal", "err", lockErr)
		return
	}
	if fileLock != nil {
		defer func() { _ = fileLock.Close() }()
	}

	// 新規 until_encoded 候補は列挙後に作り直し要求が追加される可能性がある。
	// API と同じ recordings 行ロックを取り、要求が commit されていないことを
	// 名前付き述語で再確認してから deleting にする。
	if source == "until_encoded" {
		marked, err := w.MediaDelete.MarkUntilEncodedDeleting(ctx, t.RecordingID, t.MediaAssetID, t.ID)
		if err != nil {
			log.Error("delete_reconcile: rechecking until_encoded eligibility", "err", err)
			return
		}
		if !marked {
			return
		}
	} else if source != "pending" {
		// active からの遷移だけを記録する。pending は既に deleting である。
		marked, err := w.MediaDelete.MarkDeleting(ctx, t.ID, "active")
		if err != nil {
			log.Error("delete_reconcile: marking asset deleting", "err", err)
			return
		}
		if !marked {
			return
		}
	}
	beforeMediaAssetUnlink(t.RelPath)
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking media asset"); err != nil {
		log.Warn("delete_reconcile: withholding asset unlink because media store marker did not match", "err", err)
		return
	}

	// ErrNotExist は「既に消えた」として扱い、下の MarkDeleted の直前の照合で事実として確定する。
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		// unlink 失敗。行は deleting のまま残し、次パスで再試行する。
		log.Error("delete_reconcile: removing file", "err", removeErr)
		return
	}

	// VOD の字幕は encoded アセットに隣接する sidecar で、独立した
	// media_assets 行を持たない。encoded 本体を削除する段階で同時に削除し、
	// tombstone が残る間に orphan 判定から除外され続けるリークを防ぐ。
	//
	// **失敗しても継続する（return しない）。** サイドカーは主資産ではない
	// --- rel_path に拡張子が無い、権限で消せない等の理由で 3 経路のどれかが
	// 失敗しても、それを理由に encoded 本体（映像そのもの）を永久に回収不能に
	// してはならない。失敗は log のみで、サイドカーは canonical 孤児として残り、
	// aging 後に報告される（TestDeleteReconcileWorker_CanonicalAssetKindsAreReportOnly）。
	if t.Kind == store.AssetKindEncoded {
		if subtitleRelPath, pathErr := mediapath.SubtitleSibling(t.RelPath); pathErr != nil {
			log.Warn("delete_reconcile: building subtitle sidecar path; continuing without removing it", "err", pathErr)
		} else if subtitlePath, resolveErr := mediapath.Resolve(w.MediaDir, subtitleRelPath); resolveErr != nil {
			log.Warn("delete_reconcile: resolving subtitle sidecar path; continuing without removing it", "err", resolveErr)
		} else if markerErr := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking subtitle sidecar"); markerErr != nil {
			log.Warn("delete_reconcile: withholding asset deletion because media store marker did not match", "err", markerErr)
			return
		} else if removeErr := os.Remove(subtitlePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Warn("delete_reconcile: removing subtitle sidecar; continuing", "err", removeErr)
		}
	}

	// 本体と sidecar の ErrNotExist も、unlink の成功と同じくこの 1 回の照合で deleted にしてよいと確かめる。
	beforeMediaAssetMarkDeleted(t.ID)
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "marking media asset deleted"); err != nil {
		log.Warn("delete_reconcile: withholding deleted state because media store marker did not match", "err", err)
		return
	}
	if err := w.MediaDelete.MarkDeleted(ctx, t.ID); err != nil {
		log.Error("delete_reconcile: marking asset deleted", "err", err)
		return
	}

	metrics.DeleteReconcileDeleted.WithLabelValues(source).Inc()
	metrics.DeleteReconcileBytes.WithLabelValues(source).Add(float64(t.SizeBytes))
	log.Info("delete_reconcile: deleted asset")
}

// beforeMediaAssetUnlink is a test hook at the state-change/unlink boundary.
// Production work proceeds immediately.
var beforeMediaAssetUnlink = func(string) {}

// beforeMediaAssetMarkDeleted はファイル削除後・DB の deleted 遷移前のテスト用同期点。
// 本番では何もしない。
var beforeMediaAssetMarkDeleted = func(int64) {}

// beforeOrphanFileUnlink は孤児ファイルの再確認後・unlink 前のテスト用同期点。
// 本番では何もしない。
var beforeOrphanFileUnlink = func(string) {}

// afterOrphanFileUnlink は孤児ファイルの unlink 後・「既に無かった」を DB へ記録する前の
// テスト用同期点。本番では何もしない。
var afterOrphanFileUnlink = func(string) {}

// afterDeletingAssetStat は deleting 行の Stat 後・MarkDeleted / RevertToActive の前のテスト用同期点。
// 本番では何もしない。
var afterDeletingAssetStat = func(string) {}

// resolveUnqualifiedDeletingAsset は MediaDelete.ListUnqualifiedDeletingAssets が挙げた
// 1 行（trash / until_encoded のどちらの判定にも該当しなくなった deleting
// 行）を、ファイルの現存を確認してから active か deleted のどちらかに
// 決着させる（issue #105）。
//
// ファイルがまだ存在すれば、判定条件を MediaDelete.RevertToActive で
// 再評価しつつ active に戻す（不変条件 9「適用の瞬間」。ここまでの SELECT →
// stat の間に別の書き手が recordings 側を書き換えて再度条件を満たすように
// なっていれば、0 行のまま deleting が保たれ、pending 経路が続行する）。
//
// ファイルが既に無ければ（unlink 成功後 MediaDelete.MarkDeleted がコミットされる
// 前にプロセスが落ち、その間に復元された）、active には戻さず deleted を
// 確定する。ここで無条件に active へ戻すと、案 B（復元時に deleting を
// 同期的に active へ戻す）を却下した理由そのもの ——「active なのにファイルが
// 無い行」を作ってしまう。
func (w *DeleteReconcileWorker) resolveUnqualifiedDeletingAsset(ctx context.Context, a store.MediaDeleteRevertCandidate, graceCutoff, overlapCutoff time.Time, placementRules []byte, placementDerived string) {
	log := slog.With("media_asset_id", a.MediaAssetID, "media_asset_copy_id", a.ID, "recording_id", a.RecordingID, "rel_path", a.RelPath)

	var statErr error
	if a.Kind != store.AssetKindOriginal {
		if w.DerivedObjects == nil {
			log.Error("delete_reconcile: derived object store is not configured while resolving revert candidate")
			return
		}
		_, statErr = w.DerivedObjects.Head(ctx, a.RelPath)
		if errors.Is(statErr, objstore.ErrNotFound) {
			statErr = os.ErrNotExist
		}
	} else {
		path, err := mediapath.Resolve(w.MediaDir, a.RelPath)
		if err != nil {
			log.Error("delete_reconcile: rejecting rel_path outside the media directory while resolving revert candidate", "err", err)
			return
		}
		_, statErr = os.Stat(path)
	}
	afterDeletingAssetStat(a.RelPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "confirming deleting media asset is absent"); err != nil {
				log.Warn("delete_reconcile: withholding missing-file decision because media store marker did not match", "err", err)
				return
			}
			if err := w.MediaDelete.MarkDeleted(ctx, a.ID); err != nil {
				log.Error("delete_reconcile: finalizing already-unlinked asset as deleted", "err", err)
				return
			}
			log.Warn("delete_reconcile: file already removed before revert could apply; finalized as deleted instead of reverting to active")
			return
		}
		// stat 自体が失敗（権限など）。存在有無を確定できないので何もせず
		// deleting のまま残し、次パスで再評価する。
		log.Error("delete_reconcile: statting revert candidate, leaving deleting for retry", "err", statErr)
		return
	}

	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "reverting deleting media asset"); err != nil {
		log.Warn("delete_reconcile: withholding deleting-state resolution because media store marker did not match", "err", err)
		return
	}
	updated, err := w.MediaDelete.RevertToActive(ctx, a.ID, graceCutoff, overlapCutoff, placementRules, placementDerived)
	if err != nil {
		log.Error("delete_reconcile: reverting asset to active", "err", err)
		return
	}
	if updated {
		log.Info("delete_reconcile: reverted deleting asset to active (no longer qualifies for deletion)")
	}
}

// deleteOrphanFile は公開前と判定できる孤児だけを物理削除し orphan_files 行を消す。
// canonical 名はここへ来ても削除しない。ingest temp/checkpoint は同じ flock を取り、
// staging は一意な予約名なので lock を取らず、mtime を再確認してから unlink する。
func (w *DeleteReconcileWorker) deleteOrphanFile(relPath string, mtimeGrace time.Duration) {
	log := slog.With("rel_path", relPath, "source", "orphan")
	kind := classifyOrphanCleanup(relPath)
	if kind == orphanCleanupCanonical {
		// Work はここへ来ない（agedUnpublishedOrphans が canonical 名を先に除く）。
		// 直接の呼び出し元が canonical 名を unlink しないための防御として残している。
		return
	}
	if kind == orphanCleanupStaging && mediapath.IsGeneratedAssetTempFile(filepath.Base(relPath)) {
		w.deleteGeneratedAssetStagingOrphan(relPath, mtimeGrace, log)
		return
	}

	path, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting orphan rel_path outside the media directory", "err", err)
		return
	}
	if kind == orphanCleanupIngestCheckpoint {
		w.deleteOrphanIngestCheckpoint(path, relPath, mtimeGrace, log)
		return
	}
	if kind == orphanCleanupTransferCheckpoint {
		w.deleteOrphanTransferCheckpoint(path, relPath, mtimeGrace, log)
		return
	}

	isTransferTemp := kind == orphanCleanupTransferTemp
	isTemp := kind == orphanCleanupIngestTemp || isTransferTemp
	var tempLock *os.File
	var checkpointPath, checkpointRelPath string
	if isTemp {
		var ok bool
		if isTransferTemp {
			tempLock, ok = w.lockOrphanTransferTemp(path, relPath, log)
		} else {
			tempLock, ok = w.lockOrphanIngestTemp(path, relPath, log)
		}
		if !ok {
			return
		}
		defer closeOrphanTempLock(tempLock, log)
		// A recent checkpoint means active work still owns this temp. Keep the temp
		// mtime unchanged because it anchors the fallback SHA-256 wait deadline.
		checkpointPath = ingestCheckpointPath(path)
		checkpointRelPath = relPath + ingestCheckpointSuffix
		if isTransferTemp {
			checkpointPath = fsink.CheckpointPath(path)
			checkpointRelPath = relPath + fsink.CheckpointSuffix
		}
		checkpointInfo, err := os.Stat(checkpointPath)
		if err == nil && checkpointInfo.ModTime().After(time.Now().Add(-mtimeGrace)) {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("delete_reconcile: stating temp checkpoint before temp cleanup", "err", err)
			return
		}
	}

	size, missing, ready := inspectAgedOrphan(path, tempLock, isTemp, mtimeGrace, log)
	if !ready {
		if !missing {
			return
		}
		// ロック取得後にパスが消えた temp から os.Remove へ進むと、同名で再作成された
		// 新しい temp をロックなしで消しうる。temp も staging も、候補記録だけを整理する。
		w.clearAbsentOrphanRecord(relPath, log)
		return
	}

	beforeOrphanFileUnlink(relPath)
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking orphan file"); err != nil {
		log.Warn("delete_reconcile: withholding orphan unlink because media store marker did not match", "err", err)
		return
	}
	physicallyDeleted, ok := removeOrphanFile(path, log)
	if !ok {
		return
	}
	afterOrphanFileUnlink(relPath)
	if !physicallyDeleted {
		if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "confirming orphan file is absent"); err != nil {
			log.Warn("delete_reconcile: withholding missing-orphan decision because media store marker did not match", "err", err)
			return
		}
	}

	if isTemp {
		// checkpoint は base temp と同じ寿命である。base の flock を保持したまま
		// 消し、古い hash 状態を次の同名 temp が誤って読む経路を閉じる。
		// 直前の temp の unlink と同じ照合で守られている。
		removeCheckpointErr := os.Remove(checkpointPath)
		if removeCheckpointErr != nil && !errors.Is(removeCheckpointErr, os.ErrNotExist) {
			log.Warn("delete_reconcile: removing checkpoint with aged temp failed", "err", removeCheckpointErr)
		} else {
			if err := w.Storage.DeleteOrphanFile(context.Background(), checkpointRelPath); err != nil {
				log.Warn("delete_reconcile: clearing checkpoint orphan row", "err", err)
			}
		}
		w.clearOrphanFileRecord(relPath, size, physicallyDeleted, log)
		return
	}
	w.clearOrphanFileRecord(relPath, size, physicallyDeleted, log)
}

func (w *DeleteReconcileWorker) deleteGeneratedAssetStagingOrphan(key string, mtimeGrace time.Duration, log *slog.Logger) {
	if w.DerivedObjects == nil {
		log.Error("delete_reconcile: derived object store is not configured for generated staging key")
		return
	}
	object, err := w.DerivedObjects.Head(context.Background(), key)
	if errors.Is(err, objstore.ErrNotFound) {
		w.clearAbsentOrphanRecord(key, log)
		return
	}
	if err != nil {
		metrics.RecordObjectStoreError("head", err)
		log.Warn("delete_reconcile: checking generated staging key", "err", err)
		return
	}
	if object.LastModified.After(time.Now().Add(-mtimeGrace)) {
		return
	}
	beforeOrphanFileUnlink(key)
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking generated staging key"); err != nil {
		log.Warn("delete_reconcile: withholding generated staging delete because media store marker did not match", "err", err)
		return
	}
	if err := w.DerivedObjects.Delete(context.Background(), key); err != nil {
		metrics.RecordObjectStoreError("delete", err)
		log.Error("delete_reconcile: deleting generated staging key", "err", err)
		return
	}
	afterOrphanFileUnlink(key)
	w.clearOrphanFileRecord(key, object.Size, true, log)
}

// deleteOrphanTransferCheckpoint は source temp の flock の下で checkpoint と atomic checkpoint temp を回収する。
func (w *DeleteReconcileWorker) deleteOrphanTransferCheckpoint(path, relPath string, mtimeGrace time.Duration, log *slog.Logger) {
	base := filepath.Base(path)
	basePath := path
	if checkpointAt := strings.Index(base, fsink.CheckpointSuffix); checkpointAt >= 0 {
		basePath = filepath.Join(filepath.Dir(path), base[:checkpointAt])
	}
	tempLock, err := fsink.LockExistingTempFile(basePath)
	if err == nil {
		defer closeOrphanTempLock(tempLock, log)
	} else if !errors.Is(err, os.ErrNotExist) {
		if fsink.TempLockBusy(err) {
			log.Debug("delete_reconcile: transfer temp is locked; deferring checkpoint removal")
			return
		}
		log.Warn("delete_reconcile: locking transfer temp before checkpoint removal", "err", err)
		return
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		w.clearAbsentOrphanRecord(relPath, log)
		return
	}
	if err != nil {
		log.Warn("delete_reconcile: stating transfer checkpoint", "err", err)
		return
	}
	if info.ModTime().After(time.Now().Add(-mtimeGrace)) {
		return
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking transfer checkpoint"); err != nil {
		log.Warn("delete_reconcile: withholding transfer checkpoint unlink because the store marker did not match", "err", err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("delete_reconcile: removing transfer checkpoint", "err", err)
		return
	}
	w.clearOrphanFileRecord(relPath, info.Size(), true, log)
}

func (w *DeleteReconcileWorker) lockOrphanTransferTemp(path, relPath string, log *slog.Logger) (*os.File, bool) {
	tempLock, err := fsink.LockExistingTempFile(path)
	if err == nil {
		return tempLock, true
	}
	if errors.Is(err, os.ErrNotExist) {
		w.clearAbsentOrphanRecord(relPath, log)
		return nil, false
	}
	if fsink.TempLockBusy(err) {
		log.Debug("delete_reconcile: transfer temp is locked; deferring orphan removal")
		return nil, false
	}
	log.Warn("delete_reconcile: locking transfer temp before orphan removal", "err", err)
	return nil, false
}

// deleteOrphanIngestCheckpoint は SHA-256 状態を base temp の flock の下で回収する。
// checkpoint の削除だけでも次回 replay の量が増えるだけだが、temp と同じ寿命で扱う。
func (w *DeleteReconcileWorker) deleteOrphanIngestCheckpoint(path, relPath string, mtimeGrace time.Duration, log *slog.Logger) {
	tempPath := ingestTempPathForCheckpoint(path)
	tempLock, err := lockExistingIngestTempFile(tempPath)
	if err == nil {
		defer closeOrphanTempLock(tempLock, log)
	} else if !errors.Is(err, os.ErrNotExist) {
		if ingestTempLockBusy(err) {
			log.Debug("delete_reconcile: ingest temp is locked; deferring checkpoint removal")
			return
		}
		log.Warn("delete_reconcile: locking ingest temp before checkpoint removal", "err", err)
		return
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		w.clearAbsentOrphanRecord(relPath, log)
		return
	}
	if err != nil {
		log.Warn("delete_reconcile: stating ingest checkpoint", "err", err)
		return
	}
	if info.ModTime().After(time.Now().Add(-mtimeGrace)) {
		return
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "unlinking ingest checkpoint"); err != nil {
		log.Warn("delete_reconcile: withholding checkpoint unlink because media store marker did not match", "err", err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("delete_reconcile: removing ingest checkpoint", "err", err)
		return
	}
	w.clearOrphanFileRecord(relPath, info.Size(), true, log)
}

// lockOrphanIngestTemp は ingest と同じ flock を temp ファイルに取得する。
// パスが消えた場合は再作成された別 inode を無 lock で消さないため、DB 行だけ
// を整理して呼び出し元に false を返す。busy やその他の lock エラーも安全側で
// 削除を延期する。
func (w *DeleteReconcileWorker) lockOrphanIngestTemp(path, relPath string, log *slog.Logger) (*os.File, bool) {
	tempLock, err := lockExistingIngestTempFile(path)
	if err == nil {
		return tempLock, true
	}
	if errors.Is(err, os.ErrNotExist) {
		w.clearAbsentOrphanRecord(relPath, log)
		return nil, false
	}
	if ingestTempLockBusy(err) {
		log.Debug("delete_reconcile: ingest temp is locked; deferring orphan removal")
		return nil, false
	}
	log.Warn("delete_reconcile: locking ingest temp before orphan removal", "err", err)
	return nil, false
}

// closeOrphanTempLock は orphan cleanup が取得した temp lock を閉じる。
func closeOrphanTempLock(tempLock *os.File, log *slog.Logger) {
	if err := tempLock.Close(); err != nil {
		log.Warn("delete_reconcile: closing locked ingest temp", "err", err)
	}
}

// inspectAgedOrphan は unlink 直前に孤児の mtime とサイズを再確認する。
// ingest temp では flock と inode も照合する。missing は stat が ENOENT だった場合だけ true。
func inspectAgedOrphan(path string, tempLock *os.File, isTemp bool, mtimeGrace time.Duration, log *slog.Logger) (size int64, missing, ready bool) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return 0, true, false
		}
		log.Error("delete_reconcile: stating orphan file", "err", statErr)
		return 0, false, false
	}
	if isTemp && tempLock != nil {
		lockedInfo, lockStatErr := tempLock.Stat()
		if lockStatErr != nil || !os.SameFile(lockedInfo, info) {
			// パスがロック取得後に差し替わった。別 inode をロックなしで
			// 消さず、次の pass で現在の temp を取り直す。
			log.Debug("delete_reconcile: ingest temp inode changed while locking; deferring orphan removal")
			return 0, false, false
		}
	}
	if info.ModTime().After(time.Now().Add(-mtimeGrace)) {
		// 候補抽出後に書き込みが進んだ。次の reconcile pass で再確認する。
		log.Debug("delete_reconcile: orphan became too new; deferring orphan removal")
		return 0, false, false
	}
	return info.Size(), false, true
}

// removeOrphanFile は再確認済みの公開前ファイルを unlink し、ENOENT は既に消えた成功として
// 扱う。その他の unlink 失敗では orphan_files 行を残して次回の pass に委ねる。
func removeOrphanFile(path string, log *slog.Logger) (physicallyDeleted, ok bool) {
	if err := os.Remove(path); err == nil {
		return true, true
	} else if errors.Is(err, os.ErrNotExist) {
		return false, true
	} else {
		log.Error("delete_reconcile: removing orphan file", "err", err)
		return false, false
	}
}

// clearOrphanFileRecord は orphan_files の物理削除後（または対象が既に消えて
// いた場合）に DB の記録を整理する。ctx 取消後も削除の事実を記録するため
// Background 由来にするが、DB 詰まりで worker の終了を無期限に待たない。
//
// 置き場の目印は呼び出し側が照合する。unlink の直前と、「無い」を観測した直後の
// clearAbsentOrphanRecord が守る。
func (w *DeleteReconcileWorker) clearOrphanFileRecord(relPath string, size int64, physicallyDeleted bool, log *slog.Logger) {
	// ctx 取消後も削除の事実を記録するため Background 由来にするが、DB 詰まりで worker の終了を無期限に待たない。
	cleanupCtx, cancel := context.WithTimeout(context.Background(), deleteOrphanCleanupTimeout)
	defer cancel()
	if err := w.Storage.DeleteOrphanFile(cleanupCtx, relPath); err != nil {
		log.Error("delete_reconcile: clearing orphan record after delete", "err", err)
		return
	}
	recordOrphanCleanup(log, size, physicallyDeleted)
}

// clearAbsentOrphanRecord は Stat / open が ErrNotExist だった孤児の記録を、目印を照合してから消す。
// 目印が合わない置き場では「無い」が別の置き場の観測かもしれないので、記録を消さない。
func (w *DeleteReconcileWorker) clearAbsentOrphanRecord(relPath string, log *slog.Logger) {
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "confirming orphan file is absent"); err != nil {
		log.Warn("delete_reconcile: withholding missing-orphan decision because media store marker did not match", "err", err)
		return
	}
	w.clearOrphanFileRecord(relPath, 0, false, log)
}

func recordOrphanCleanup(log *slog.Logger, size int64, physicallyDeleted bool) {
	if physicallyDeleted {
		metrics.DeleteReconcileDeleted.WithLabelValues("orphan").Inc()
		metrics.DeleteReconcileBytes.WithLabelValues("orphan").Add(float64(size))
		log.Info("delete_reconcile: deleted orphan file")
		return
	}
	log.Info("delete_reconcile: cleared orphan record; file was already absent")
}
