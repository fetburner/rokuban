package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

const (
	// defaultDeleteReconcileInterval は削除 reconcile 定期パスの既定間隔。
	defaultDeleteReconcileInterval = 15 * time.Minute

	// deleteReconcileTimeout は 1 パス（ファイル走査 + 物理 unlink 多数）の上限。
	// record_sweep と同じ理由（River 既定の 1 分では I/O 量に対して短すぎる）で
	// 長めに与える。
	deleteReconcileTimeout = 10 * time.Minute

	// defaultTrashRetention はごみ箱（recordings.deleted_at）の既定猶予（30 日）。
	defaultTrashRetention = 30 * 24 * time.Hour

	// defaultOrphanMTimeGrace は孤児候補にするまでの既定 mtime 猶予（7 日）。
	defaultOrphanMTimeGrace = 7 * 24 * time.Hour

	// defaultOrphanAge は孤児候補が実削除されるまでの既定エイジング期間（14 日）。
	defaultOrphanAge = 14 * 24 * time.Hour

	// defaultDeleteReconcileMaxPerPass は一括削除サーキットブレーカーの既定閾値。
	defaultDeleteReconcileMaxPerPass = 100

	// deleteReconcileRowLimit はソースごとに 1 パスで拾う行数の上限。
	// 際限なく積み上げてタイムアウトするのを避けるための安全弁で、
	// サーキットブレーカーの閾値（既定 100）より十分大きく取る。
	deleteReconcileRowLimit = 5000
)

// DeleteReconcileArgs は削除 reconcile ジョブの引数（issue #70、
// docs/storage.md §7）。物理ストレージは site に従属しない単一の media_dir
// なので、他の定期ジョブと異なり site 引数を持たない
// （CatalogExportArgs と同じ位置づけ）。
type DeleteReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (DeleteReconcileArgs) Kind() string { return "delete_reconcile" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// 同一引数の同時実行を UniqueOpts で防ぐ。ByState は pendingJobStates に絞る
// （completed を含めると定期ジョブが実質ワンショットになる）。
func (DeleteReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: river.QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// DeleteReconcileWorker は物理 unlink に至る 3 ソース（ごみ箱 / until_encoded /
// 孤児）を 1 本の reconcile ループに統一する River ワーカー（docs/storage.md §7）。
//
// 削除プロトコルは冪等: media_assets.state を active → deleting → deleted と
// 遷移させる。deleting のまま落ちても次パスの ListMediaAssetsPendingDelete が
// 拾い直すので、途中失敗しても残骸は蓄積しない。
//
// mirakc の basedir には一切触れない（不変条件。ここで扱うのは Rokuban 自身の
// MediaDir 配下のファイルのみ）。
type DeleteReconcileWorker struct {
	river.WorkerDefaults[DeleteReconcileArgs]
	Pool     *pgxpool.Pool
	MediaDir string

	// Site は config.mirakc.site（issue #31）。サーキットブレーカーのキーに使う。
	// 空なら db.DefaultSite を使う（テストの部分構成を許す）。
	Site string

	TrashRetention    time.Duration
	OrphanMTimeGrace  time.Duration
	OrphanAge         time.Duration
	MaxDeletesPerPass int
}

// Timeout は River の既定（1 分）より長い上限を与える。
func (w *DeleteReconcileWorker) Timeout(*river.Job[DeleteReconcileArgs]) time.Duration {
	return deleteReconcileTimeout
}

// Work は 1 パス分の削除 reconcile を実行する。
func (w *DeleteReconcileWorker) Work(ctx context.Context, _ *river.Job[DeleteReconcileArgs]) error {
	site := w.Site
	if site == "" {
		site = db.DefaultSite
	}
	trashRetention := w.TrashRetention
	if trashRetention <= 0 {
		trashRetention = defaultTrashRetention
	}
	orphanMTimeGrace := w.OrphanMTimeGrace
	if orphanMTimeGrace <= 0 {
		orphanMTimeGrace = defaultOrphanMTimeGrace
	}
	orphanAge := w.OrphanAge
	if orphanAge <= 0 {
		orphanAge = defaultOrphanAge
	}
	maxPerPass := w.MaxDeletesPerPass
	if maxPerPass <= 0 {
		maxPerPass = defaultDeleteReconcileMaxPerPass
	}

	q := sqlcgen.New(w.Pool)

	// パスの先頭でブレーカーの発動状態を DB の真実に合わせ直す
	// （ObserveState のコメント参照。プロセス再起動でゲージが失われるため）。
	tripped, err := breaker.ObserveState(ctx, q, site, breaker.DeleteReconcile)
	if err != nil {
		return fmt.Errorf("observing %s circuit breaker: %w", breaker.DeleteReconcile, err)
	}

	// 前パスで deleting のまま止まった行を最優先で再開する。これは「既に決めた
	// 削除」の再実行であり新規の判断ではないため、サーキットブレーカーの対象外
	// （docs/storage.md §7「どこで落ちても reconcile が拾い直す」）。
	pending, err := q.ListMediaAssetsPendingDelete(ctx, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing pending deletes: %w", err)
	}
	for _, a := range pending {
		w.deleteMediaAsset(ctx, q, a.ID, a.RelPath, a.SizeBytes, "pending")
	}

	// 孤児候補の記録/解除はファイルを消さないので、ブレーカーとは無関係に毎回行う。
	if err := w.reconcileOrphanCandidates(ctx, q, orphanMTimeGrace); err != nil {
		return fmt.Errorf("reconciling orphan candidates: %w", err)
	}

	trashCutoff := time.Now().Add(-trashRetention)
	trashRows, err := q.ListTrashMediaAssetsToDelete(ctx, sqlcgen.ListTrashMediaAssetsToDeleteParams{
		GraceCutoff: trashCutoff,
		RowLimit:    deleteReconcileRowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing trash assets past retention: %w", err)
	}

	untilEncodedCandidates, err := q.ListUntilEncodedOriginalsToDelete(ctx, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing until_encoded originals: %w", err)
	}
	untilEncodedRows := make([]sqlcgen.ListUntilEncodedOriginalsToDeleteRow, 0, len(untilEncodedCandidates))
	for _, r := range untilEncodedCandidates {
		hasPending, err := w.hasPendingDerivativeJob(ctx, r.RecordingID)
		if err != nil {
			return fmt.Errorf("checking pending derivative jobs for recording %d: %w", r.RecordingID, err)
		}
		if hasPending {
			continue
		}
		untilEncodedRows = append(untilEncodedRows, r)
	}

	agedOrphans, err := w.verifiedAgedOrphans(ctx, q, orphanAge, orphanMTimeGrace)
	if err != nil {
		return fmt.Errorf("verifying aged orphans: %w", err)
	}

	total := len(trashRows) + len(untilEncodedRows) + len(agedOrphans)
	if total > maxPerPass {
		sample := breaker.Sample{Total: total}
		if tripErr := breaker.Trip(ctx, q, site, breaker.DeleteReconcile, maxPerPass, sample); tripErr != nil {
			return fmt.Errorf("tripping circuit breaker: %w", tripErr)
		}
		metrics.DeleteReconcileLastPass.SetToCurrentTime()
		return nil
	}
	if tripped {
		if total > 0 {
			slog.Warn("delete_reconcile: circuit breaker latched — withholding new deletes until manually resumed",
				"breaker", breaker.DeleteReconcile, "pending_deletes", total)
		}
		metrics.DeleteReconcileLastPass.SetToCurrentTime()
		return nil
	}

	for _, a := range trashRows {
		w.deleteMediaAsset(ctx, q, a.ID, a.RelPath, a.SizeBytes, "trash")
	}
	for _, a := range untilEncodedRows {
		w.deleteMediaAsset(ctx, q, a.ID, a.RelPath, a.SizeBytes, "until_encoded")
	}
	for _, relPath := range agedOrphans {
		w.deleteOrphanFile(q, relPath)
	}

	metrics.DeleteReconcileLastPass.SetToCurrentTime()
	return nil
}

// deleteMediaAsset は 1 アセットを冪等な 3 段階（active → deleting → unlink →
// deleted）で物理削除する。各段階のエラーはログに残すだけで呼び出し元には
// 伝播しない — 1 件の失敗でパス全体を止めると、後続の正常な削除まで巻き込む
// ため（record_sweep の processRecord と同じ設計判断）。deleting のまま
// 終わった行は次パスの ListMediaAssetsPendingDelete が拾い直す。
func (w *DeleteReconcileWorker) deleteMediaAsset(ctx context.Context, q *sqlcgen.Queries, id int64, relPath string, sizeBytes int64, source string) {
	log := slog.With("media_asset_id", id, "rel_path", relPath, "source", source)

	// 既に deleting なら 0 行更新（それでよい。pending 経路はこの UPDATE を
	// スキップしても問題ない冪等な no-op）。active からの遷移だけを記録する。
	if _, err := q.MarkMediaAssetDeleting(ctx, id); err != nil {
		log.Error("delete_reconcile: marking asset deleting", "err", err)
		return
	}

	path, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting rel_path outside the media directory", "err", err)
		return
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// unlink 失敗。行は deleting のまま残し、次パスで再試行する。
		log.Error("delete_reconcile: removing file", "err", err)
		return
	}

	if _, err := q.MarkMediaAssetDeleted(ctx, id); err != nil {
		log.Error("delete_reconcile: marking asset deleted", "err", err)
		return
	}

	metrics.DeleteReconcileDeleted.WithLabelValues(source).Inc()
	metrics.DeleteReconcileBytes.WithLabelValues(source).Add(float64(sizeBytes))
	log.Info("delete_reconcile: deleted asset")
}

// hasPendingDerivativeJob は原本を入力とする encode/thumbnail ジョブが
// 実行中・再試行待ちでないかを確認する（docs/storage.md §7「原本を入力とする
// 実行中・再試行中のジョブがない」）。river_job は rivermigrate が管理する
// テーブルで sqlc のスキーマディレクトリには含まれないため、生 SQL で問い合わせる。
func (w *DeleteReconcileWorker) hasPendingDerivativeJob(ctx context.Context, recordingID int64) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1 FROM river_job
			WHERE kind IN ('encode', 'thumbnail')
			  AND state IN ('available', 'pending', 'scheduled', 'retryable', 'running')
			  AND (args ->> 'recording_id')::bigint = $1
		)`
	var pending bool
	if err := w.Pool.QueryRow(ctx, query, recordingID).Scan(&pending); err != nil {
		return false, err
	}
	return pending, nil
}

// reconcileOrphanCandidates は MediaDir を走査し、media_assets のどの行からも
// 参照されていないファイルを孤児候補として orphan_files に記録する。
// mtime が新しいファイル（OrphanMTimeGrace 以内）は候補にしない。
// 既に孤児でなくなった（登録された、またはファイルが消えた）行は掃除する。
// ファイルは一切消さない（記録のみ）。
func (w *DeleteReconcileWorker) reconcileOrphanCandidates(ctx context.Context, q *sqlcgen.Queries, mtimeGrace time.Duration) error {
	known, err := q.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return fmt.Errorf("listing known rel paths: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known))
	for _, k := range known {
		knownSet[k] = struct{}{}
	}

	mtimeCutoff := time.Now().Add(-mtimeGrace)
	candidates := make(map[string]struct{})
	if err := walkMediaFiles(w.MediaDir, func(relPath string, info fs.FileInfo) {
		if _, ok := knownSet[relPath]; ok {
			return
		}
		if info.ModTime().After(mtimeCutoff) {
			return
		}
		candidates[relPath] = struct{}{}
	}); err != nil {
		return fmt.Errorf("walking media dir: %w", err)
	}

	for relPath := range candidates {
		if err := q.UpsertOrphanFile(ctx, relPath); err != nil {
			return fmt.Errorf("recording orphan candidate %q: %w", relPath, err)
		}
	}

	existing, err := q.ListAllOrphanFiles(ctx)
	if err != nil {
		return fmt.Errorf("listing orphan files: %w", err)
	}
	for _, o := range existing {
		if _, stillCandidate := candidates[o.RelPath]; stillCandidate {
			continue
		}
		// もう孤児候補でない（media_assets に登録された、mtime が新しくなった
		// はずはないが再走査で見えなくなった＝ファイルが消えた等）。掃除する。
		if err := q.DeleteOrphanFile(ctx, o.RelPath); err != nil {
			return fmt.Errorf("clearing stale orphan record %q: %w", o.RelPath, err)
		}
	}
	return nil
}

// verifiedAgedOrphans はエイジング済みの孤児候補のうち、削除の全条件を
// 実削除の直前に再検証したものだけを返す。first_seen 時点の判定と実削除の
// 間に時間差があるため（次パスまで最大 defaultDeleteReconcileInterval）、
// その間に登録され直った・ファイルが無くなった等の変化を取りこぼさない。
func (w *DeleteReconcileWorker) verifiedAgedOrphans(ctx context.Context, q *sqlcgen.Queries, age, mtimeGrace time.Duration) ([]string, error) {
	agedPaths, err := q.ListAgedOrphanFiles(ctx, time.Now().Add(-age))
	if err != nil {
		return nil, fmt.Errorf("listing aged orphan files: %w", err)
	}
	if len(agedPaths) == 0 {
		return nil, nil
	}

	known, err := q.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing known rel paths: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known))
	for _, k := range known {
		knownSet[k] = struct{}{}
	}

	mtimeCutoff := time.Now().Add(-mtimeGrace)
	verified := make([]string, 0, len(agedPaths))
	for _, relPath := range agedPaths {
		if _, ok := knownSet[relPath]; ok {
			// 登録され直っていた。孤児ではなくなったので記録から外す。
			if err := q.DeleteOrphanFile(ctx, relPath); err != nil {
				return nil, fmt.Errorf("clearing reclaimed orphan record %q: %w", relPath, err)
			}
			continue
		}
		path, err := mediapath.Resolve(w.MediaDir, relPath)
		if err != nil {
			if err := q.DeleteOrphanFile(ctx, relPath); err != nil {
				return nil, fmt.Errorf("clearing invalid orphan record %q: %w", relPath, err)
			}
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				if err := q.DeleteOrphanFile(ctx, relPath); err != nil {
					return nil, fmt.Errorf("clearing vanished orphan record %q: %w", relPath, err)
				}
				continue
			}
			return nil, fmt.Errorf("statting orphan file %q: %w", relPath, err)
		}
		if info.ModTime().After(mtimeCutoff) {
			// mtime が更新されていた（誰かが触った）。安全側で見送る。
			continue
		}
		verified = append(verified, relPath)
	}
	return verified, nil
}

// deleteOrphanFile は孤児ファイルを物理削除し orphan_files 行を消す。
// media_assets に対応する行はそもそも無いので deleting → deleted の遷移はない。
func (w *DeleteReconcileWorker) deleteOrphanFile(q *sqlcgen.Queries, relPath string) {
	log := slog.With("rel_path", relPath, "source", "orphan")

	path, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting orphan rel_path outside the media directory", "err", err)
		return
	}

	var size int64
	if info, statErr := os.Stat(path); statErr == nil {
		size = info.Size()
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("delete_reconcile: removing orphan file", "err", err)
		return
	}

	if err := q.DeleteOrphanFile(context.Background(), relPath); err != nil {
		log.Error("delete_reconcile: clearing orphan record after delete", "err", err)
		return
	}

	metrics.DeleteReconcileDeleted.WithLabelValues("orphan").Inc()
	metrics.DeleteReconcileBytes.WithLabelValues("orphan").Add(float64(size))
	log.Info("delete_reconcile: deleted orphan file")
}

// walkMediaFiles は mediaDir 配下の通常ファイルを列挙する。catalog.Subdir
// （災害復旧用メタデータ）はメディアアセットではないので走査から除く。
func walkMediaFiles(mediaDir string, fn func(relPath string, info fs.FileInfo)) error {
	catalogDir := filepath.Join(mediaDir, catalog.Subdir)
	return filepath.Walk(mediaDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if path == catalogDir {
				return filepath.SkipDir
			}
			return nil
		}
		relPath, err := filepath.Rel(mediaDir, path)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(relPath), info)
		return nil
	})
}
