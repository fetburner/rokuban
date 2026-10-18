// delete_execute.go は削除条件の再評価、ブレーカー、削除中行の再開と物理削除を担当する。
// 読む表は circuit_breakers、recordings、media_assets と名前付き削除述語の参照先。
// 書く表は circuit_breakers、media_assets、encoded_asset_removal_requests、orphan_files。
//
// 実行は until_encoded の述語を、実行の直前に再評価する（LockRecording と
// MarkUntilEncodedOriginalDeleting）。flock を deleting 遷移より前に取るので、
// この遷移は選定へ移せない。
//
// orphan_files の書き手は 2 者いる。候補は storage_audit.go が書き、実行が unlink の後に
// 行を消す。実行が消すのは、ctx が取り消された後も削除の事実を記録するためである。
//
// ファイル操作は次の表のとおり。字幕 sidecar は encoded 本体と同じ段階で消すので派生物、
// staging（encode / 生成アセットの予約名の一時出力）は派生物の公開前ファイル、
// checkpoint は base temp と同じ flock の下で消すので ingest temp に入れる。
//
//	操作      原本                         派生物                          ingest temp
//	解決      mediapath.Resolve            同左（字幕・staging も）         同左（checkpoint も）
//	flock     media root の rel_path lock  同左（字幕は本体の lock に含む。  temp 自身の flock
//	                                       staging は取らない）             （checkpoint は base temp の）
//	stat      巻き戻し時の現存確認         同左。staging は mtime も再確認  mtime・サイズの再確認と、
//	                                                                        flock との inode 照合
//	unlink    する                         する（encoded は字幕も）         する（checkpoint も同時）
//
// ストレージ port（#1283）の候補になるのは派生物の列だけで、原本と ingest temp の
// flock・inode 照合は常にファイルシステムに残る。asset の flock は種類を問わず同じ
// 関数（lockMediaRelPathFile）で取るので、派生物側の flock を port に出すかは #1283 で決める。

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
)

// observeDeleteReconcile は削除 reconcile のブレーカー状態を DB の真実から観測する。
//
// パスの先頭でブレーカーの発動状態を DB の真実に合わせ直す（ObserveState の
// コメント参照。プロセス再起動でゲージが失われるため）。
func (w *DeleteReconcileWorker) observeDeleteReconcile(ctx context.Context, breakers store.Breakers, site string) (bool, error) {
	tripped, err := breaker.ObserveState(ctx, breakers, site, breaker.DeleteReconcile)
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
func (w *DeleteReconcileWorker) restoreUnqualifiedDeletingAssets(ctx context.Context, q *sqlcgen.Queries, trashCutoff time.Time) error {
	rows, err := q.ListUnqualifiedDeletingAssets(ctx, sqlcgen.ListUnqualifiedDeletingAssetsParams{
		GraceCutoff: trashCutoff, RowLimit: deleteReconcileRowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing unqualified deleting assets: %w", err)
	}
	for _, a := range rows {
		w.resolveUnqualifiedDeletingAsset(ctx, q, a, trashCutoff)
	}
	return nil
}

// listPendingDeleteAssets は前回のパスで deleting のまま残ったアセットを取得する。
//
// 前パスで deleting のまま止まった行のうち、まだ trash / until_encoded の
// いずれかの判定に該当するものを最優先で再開する。これは「既に決めた
// 削除」の再実行であり新規の判断ではないため、サーキットブレーカーの対象外
// （docs/storage.md §7「どこで落ちても reconcile が拾い直す」）。
// restoreUnqualifiedDeletingAssets で該当しなくなった行は既に active か
// deleted に決着しているので、ここに現れるのは常に判定を再確認できた
// ものだけ。
func listPendingDeleteAssets(ctx context.Context, q *sqlcgen.Queries, trashCutoff time.Time) ([]sqlcgen.ListMediaAssetsPendingDeleteRow, error) {
	rows, err := q.ListMediaAssetsPendingDelete(ctx, sqlcgen.ListMediaAssetsPendingDeleteParams{
		GraceCutoff: trashCutoff, RowLimit: deleteReconcileRowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pending deletes: %w", err)
	}
	return rows, nil
}

// applyDeleteCircuitBreaker は新規削除の上限を超えたとき、または発動中に削除を止める。
func (w *DeleteReconcileWorker) applyDeleteCircuitBreaker(ctx context.Context, breakers store.Breakers, site string, tripped bool, total, maxPerPass int) (bool, error) {
	if total > maxPerPass {
		if err := breaker.Trip(ctx, breakers, site, breaker.DeleteReconcile, maxPerPass, breaker.Sample{Total: total}); err != nil {
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

// deleteCandidates はごみ箱・until_encoded・外した版と公開前の孤児候補を物理削除する。
func (w *DeleteReconcileWorker) deleteCandidates(ctx context.Context, q *sqlcgen.Queries, c deleteCandidateSet, orphanMTimeGrace time.Duration) {
	for _, a := range c.trash {
		w.deleteMediaAsset(ctx, q, deleteTarget{ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes, Kind: a.Kind}, "trash")
	}
	for _, a := range c.untilEncoded {
		w.deleteMediaAsset(ctx, q, deleteTarget{ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes, Kind: a.Kind}, "until_encoded")
	}
	for _, a := range c.removedEncoded {
		w.deleteMediaAsset(ctx, q, deleteTarget{ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes, Kind: a.Kind}, "removed_encoded")
	}
	for _, relPath := range c.agedOrphans {
		// ctx 死亡後は残りを打ち切る（DeleteOrphanFile 1 件ごとに
		// deleteOrphanCleanupTimeout を待ち切って進むと、DB が詰まった状態では
		// パス全体の締切を超えうる）。残った孤児は次パスの
		// agedUnpublishedOrphans が拾い直す（レベルトリガー、不変条件 5）。
		if ctx.Err() != nil {
			return
		}
		w.deleteOrphanFile(q, relPath, orphanMTimeGrace)
	}
}

// deleteMediaAsset は 1 アセットを冪等な 3 段階（active → deleting → unlink →
// deleted）で物理削除する。各段階のエラーはログに残すだけで呼び出し元には
// 伝播しない — 1 件の失敗でパス全体を止めると、後続の正常な削除まで巻き込む
// ため（record_sweep の processRecord と同じ設計判断）。deleting のまま
// 終わった行は次パスの ListMediaAssetsPendingDelete が拾い直す。
//
// 「録画そのものが消えたか」の判定は呼び出し元ではなく MarkPurgedRecordings
// が録画単位・パス末尾で行う（issue #135）ため、ここでは deleted まで到達
// したかどうかを戻さない。
func (w *DeleteReconcileWorker) deleteMediaAsset(ctx context.Context, q *sqlcgen.Queries, t deleteTarget, source string) {
	log := slog.With("media_asset_id", t.ID, "rel_path", t.RelPath, "source", source)

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
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			log.Error("delete_reconcile: beginning until_encoded delete", "err", err)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		txq := sqlcgen.New(tx)
		if _, err := txq.LockRecording(ctx, t.RecordingID); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				log.Error("delete_reconcile: locking recording before until_encoded delete", "err", err)
			}
			return
		}
		marked, err := txq.MarkUntilEncodedOriginalDeleting(ctx, t.ID)
		if err != nil {
			log.Error("delete_reconcile: rechecking until_encoded eligibility", "err", err)
			return
		}
		if marked == 0 {
			return
		}
		if err := tx.Commit(ctx); err != nil {
			log.Error("delete_reconcile: committing until_encoded delete", "err", err)
			return
		}
	} else {
		// 既に deleting なら 0 行更新（それでよい。pending 経路はこの UPDATE を
		// スキップしても問題ない冪等な no-op）。active からの遷移だけを記録する。
		if _, err := q.MarkMediaAssetDeleting(ctx, t.ID); err != nil {
			log.Error("delete_reconcile: marking asset deleting", "err", err)
			return
		}
	}
	beforeMediaAssetUnlink(t.RelPath)

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// unlink 失敗。行は deleting のまま残し、次パスで再試行する。
		log.Error("delete_reconcile: removing file", "err", err)
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
	if t.Kind == db.AssetKindEncoded {
		if subtitleRelPath, pathErr := mediapath.SubtitleSibling(t.RelPath); pathErr != nil {
			log.Warn("delete_reconcile: building subtitle sidecar path; continuing without removing it", "err", pathErr)
		} else if subtitlePath, resolveErr := mediapath.Resolve(w.MediaDir, subtitleRelPath); resolveErr != nil {
			log.Warn("delete_reconcile: resolving subtitle sidecar path; continuing without removing it", "err", resolveErr)
		} else if removeErr := os.Remove(subtitlePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Warn("delete_reconcile: removing subtitle sidecar; continuing", "err", removeErr)
		}
	}

	if _, err := q.MarkMediaAssetDeleted(ctx, t.ID); err != nil {
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

// resolveUnqualifiedDeletingAsset は ListUnqualifiedDeletingAssets が挙げた
// 1 行（trash / until_encoded のどちらの判定にも該当しなくなった deleting
// 行）を、ファイルの現存を確認してから active か deleted のどちらかに
// 決着させる（issue #105）。
//
// ファイルがまだ存在すれば、判定条件を RevertMediaAssetToActive の WHERE で
// 再評価しつつ active に戻す（不変条件 9「適用の瞬間」。ここまでの SELECT →
// stat の間に別の書き手が recordings 側を書き換えて再度条件を満たすように
// なっていれば、0 行のまま deleting が保たれ、pending 経路が続行する）。
//
// ファイルが既に無ければ（unlink 成功後 MarkMediaAssetDeleted がコミットされる
// 前にプロセスが落ち、その間に復元された）、active には戻さず deleted を
// 確定する。ここで無条件に active へ戻すと、案 B（復元時に deleting を
// 同期的に active へ戻す）を却下した理由そのもの ——「active なのにファイルが
// 無い行」を作ってしまう。
func (w *DeleteReconcileWorker) resolveUnqualifiedDeletingAsset(ctx context.Context, q *sqlcgen.Queries, a sqlcgen.ListUnqualifiedDeletingAssetsRow, graceCutoff time.Time) {
	log := slog.With("media_asset_id", a.ID, "recording_id", a.RecordingID, "rel_path", a.RelPath)

	path, err := mediapath.Resolve(w.MediaDir, a.RelPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting rel_path outside the media directory while resolving revert candidate", "err", err)
		return
	}

	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if _, err := q.MarkMediaAssetDeleted(ctx, a.ID); err != nil {
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

	n, err := q.RevertMediaAssetToActive(ctx, sqlcgen.RevertMediaAssetToActiveParams{
		ID:          a.ID,
		GraceCutoff: graceCutoff,
	})
	if err != nil {
		log.Error("delete_reconcile: reverting asset to active", "err", err)
		return
	}
	if n > 0 {
		log.Info("delete_reconcile: reverted deleting asset to active (no longer qualifies for deletion)")
	}
}

// deleteOrphanFile は公開前と判定できる孤児だけを物理削除し orphan_files 行を消す。
// canonical 名はここへ来ても削除しない。ingest temp/checkpoint は同じ flock を取り、
// staging は一意な予約名なので lock を取らず、mtime を再確認してから unlink する。
func (w *DeleteReconcileWorker) deleteOrphanFile(q *sqlcgen.Queries, relPath string, mtimeGrace time.Duration) {
	log := slog.With("rel_path", relPath, "source", "orphan")
	kind := classifyOrphanCleanup(relPath)
	if kind == orphanCleanupCanonical {
		// Work はここへ来ない（agedUnpublishedOrphans が canonical 名を先に除く）。
		// 直接の呼び出し元が canonical 名を unlink しないための防御として残している。
		return
	}

	path, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting orphan rel_path outside the media directory", "err", err)
		return
	}
	if kind == orphanCleanupIngestCheckpoint {
		w.deleteOrphanIngestCheckpoint(q, path, relPath, mtimeGrace, log)
		return
	}

	isTemp := kind == orphanCleanupIngestTemp
	var tempLock *os.File
	if isTemp {
		var ok bool
		tempLock, ok = w.lockOrphanIngestTemp(q, path, relPath, log)
		if !ok {
			return
		}
		defer closeOrphanTempLock(tempLock, log)
		// A recent checkpoint means a snoozed ingest still owns this temp. Keep the
		// temp mtime unchanged because it anchors the fallback SHA-256 wait deadline.
		checkpointInfo, err := os.Stat(ingestCheckpointPath(path))
		if err == nil && checkpointInfo.ModTime().After(time.Now().Add(-mtimeGrace)) {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("delete_reconcile: stating ingest checkpoint before temp cleanup", "err", err)
			return
		}
	}

	size, missing, ready := inspectAgedOrphan(path, tempLock, isTemp, mtimeGrace, log)
	if !ready {
		if !missing {
			return
		}
		if isTemp {
			// ロック取得後にパスが消えた場合も、ここから os.Remove へ進むと
			// 同名で再作成された新しい temp をロックなしで消しうる。
			w.clearOrphanFileRecord(q, relPath, 0, false, log)
			return
		}
		// ingest temp / staging が消えていれば候補記録だけを整理する。
		w.clearOrphanFileRecord(q, relPath, 0, false, log)
		return
	}

	physicallyDeleted, ok := removeOrphanFile(path, log)
	if !ok {
		return
	}

	if isTemp {
		// checkpoint は base temp と同じ寿命である。base の flock を保持したまま
		// 消し、古い hash 状態を次の同名 temp が誤って読む経路を閉じる。
		checkpointRelPath := relPath + ingestCheckpointSuffix
		if err := os.Remove(ingestCheckpointPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("delete_reconcile: removing ingest checkpoint with aged temp failed", "err", err)
		} else if err := q.DeleteOrphanFile(context.Background(), checkpointRelPath); err != nil {
			log.Warn("delete_reconcile: clearing ingest checkpoint orphan row", "err", err)
		}
		w.clearOrphanFileRecord(q, relPath, size, physicallyDeleted, log)
		return
	}
	w.clearOrphanFileRecord(q, relPath, size, physicallyDeleted, log)
}

// deleteOrphanIngestCheckpoint は SHA-256 状態を base temp の flock の下で回収する。
// checkpoint の削除だけでも次回 replay の量が増えるだけだが、temp と同じ寿命で扱う。
func (w *DeleteReconcileWorker) deleteOrphanIngestCheckpoint(q *sqlcgen.Queries, path, relPath string, mtimeGrace time.Duration, log *slog.Logger) {
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
		w.clearOrphanFileRecord(q, relPath, 0, false, log)
		return
	}
	if err != nil {
		log.Warn("delete_reconcile: stating ingest checkpoint", "err", err)
		return
	}
	if info.ModTime().After(time.Now().Add(-mtimeGrace)) {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("delete_reconcile: removing ingest checkpoint", "err", err)
		return
	}
	w.clearOrphanFileRecord(q, relPath, info.Size(), true, log)
}

// lockOrphanIngestTemp は ingest と同じ flock を temp ファイルに取得する。
// パスが消えた場合は再作成された別 inode を無 lock で消さないため、DB 行だけ
// を整理して呼び出し元に false を返す。busy やその他の lock エラーも安全側で
// 削除を延期する。
func (w *DeleteReconcileWorker) lockOrphanIngestTemp(q *sqlcgen.Queries, path, relPath string, log *slog.Logger) (*os.File, bool) {
	tempLock, err := lockExistingIngestTempFile(path)
	if err == nil {
		return tempLock, true
	}
	if errors.Is(err, os.ErrNotExist) {
		w.clearOrphanFileRecord(q, relPath, 0, false, log)
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
func (w *DeleteReconcileWorker) clearOrphanFileRecord(q *sqlcgen.Queries, relPath string, size int64, physicallyDeleted bool, log *slog.Logger) {
	// ctx 取消後も削除の事実を記録するため Background 由来にするが、DB 詰まりで worker の終了を無期限に待たない。
	cleanupCtx, cancel := context.WithTimeout(context.Background(), deleteOrphanCleanupTimeout)
	defer cancel()
	if err := q.DeleteOrphanFile(cleanupCtx, relPath); err != nil {
		log.Error("delete_reconcile: clearing orphan record after delete", "err", err)
		return
	}
	recordOrphanCleanup(log, size, physicallyDeleted)
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
