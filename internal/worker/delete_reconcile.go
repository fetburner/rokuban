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
	"github.com/fetburner/rokuban/internal/webhook"
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

	// defaultMissingAssetAge は「active なのに実体が無い」候補が確認済みとして
	// 報告されるまでの既定エイジング期間（issue #343）。孤児回収の
	// defaultOrphanAge と同じ理由（単発の走査揺れ・DB リストア直後の一時的な
	// 不整合を確認済みの異常と区別する）で持つが、削除を目的とした猶予では
	// ないため長さを揃える必要はなく、通知の遅れを抑える短めの値にしている。
	defaultMissingAssetAge = 24 * time.Hour

	// missingAssetLogBudget は 1 パスで reportAgedMissingAssets が Warn ログを
	// 個別に出す件数の上限。件数（rokuban_media_assets_missing）は既にメトリクス
	// が持っているので、ログの役目は同定（media_asset_id / rel_path）だけ ---
	// エイジング済み候補は解消するまで defaultDeleteReconcileInterval（既定
	// 15 分 = 1 日 96 パス）ごとに全件が再送されるので、上限が無ければ
	// 「候補数 × 96」行/日になる（劣化したマウントを放置すると候補数は
	// active な media_assets 全件まで伸びうる。実測はしていない算術）。
	// 超過分は件数だけを 1 行にまとめる。deleteReconcileNotifyBudget が
	// 時間の予算なのに対し、こちらは件数の予算（別物）。
	missingAssetLogBudget = 20

	// deleteReconcileRowLimit はソースごとに 1 パスで拾う行数の上限。
	// 際限なく積み上げてタイムアウトするのを避けるための安全弁で、
	// サーキットブレーカーの閾値（既定 100）より十分大きく取る。
	deleteReconcileRowLimit = 5000

	// deleteReconcileNotifyBudget は 1 パスの webhook 発火全体に与える時間上限。
	// 発火は削除がすべて終わった後に行うので削除の進捗を遅らせることはないが、
	// 通知先がハングすると 1 件あたり timeout × 2（既定 10 秒）を数十件分積み上げ、
	// deleteReconcileTimeout を食い潰してパスごと失敗させうる。webhook より
	// 本処理を優先する（M3-11）ため、超過分は捨てて件数をログに残す。
	deleteReconcileNotifyBudget = 2 * time.Minute
)

// deleteTarget は物理削除 1 件分の対象。pending / trash / until_encoded の
// 3 つの sqlc 行型は同じ列を返すので、ここで 1 つに寄せる（引数を平たく
// 並べると同型のスカラが増えて取り違えがコンパイルで検出できない）。
type deleteTarget struct {
	ID          int64
	RecordingID int64
	RelPath     string
	SizeBytes   int64
}

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
//
// Queue は cleanupQueue（issue #185 M4-13。`worker.queues` を明示的に絞れば
// 除外できるようになった、というだけで既定購読からは除外されない。
// cleanupQueue のコメント参照）。
//
// ByQueue: uniqueByQueue の理由は pendingJobStates 直後の doc コメント参照
// （river.QueueDefault → cleanup への移設自体がキュー名変更なので、同じ問題を踏む）。
func (DeleteReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: cleanupQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
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
//
// site 照合ガード（issue #139）は不要と判断: DeleteReconcileArgs は site を
// 持たない（DeleteReconcileArgs のコメント参照。物理ストレージは site に
// 従属しない単一の MediaDir）。mirakc にも触れないので、他サイトの worker が
// 拾っても「他インスタンスの id を投げる」形の壊れ方が起きない。
//
// # サーキットブレーカーのキーは常に db.DefaultSite（Site フィールドを持たない理由）
//
// このワーカーは `Site` フィールドを持たない（issue #185 M4-13 のレビューで削除）。
// cleanup キューは site 非依存の仕事を運ぶが、
// **どの worker がジョブを掴むかは site 束縛の有無に関わらない** ---
// `mirakcs: [tokyo, takamatsu]` の既定構成（0 束縛の中央プロセスを使わない
// 構成）でも、tokyo に束縛された worker と takamatsu に束縛された worker の
// どちらもこのキューを購読でき、`Deps.Site` をそのまま使うと**どちらが先に
// 掴むかでサーキットブレーカーの site 列が tokyo になるか takamatsu になるか
// 変わってしまう**。1 つの site 非依存の懸念（delete_reconcile の一括削除数）が
// 複数の site 列の下に分散して現れるのは、不変条件 12「表は行の寿命で割る」の
// 精神にも反する（この行の寿命は「delete_reconcile というジョブ種別」であって
// 「たまたま処理した worker の束縛サイト」ではない）。
//
// したがって `Deps.Site` を一切参照せず、常に db.DefaultSite
// （"default"）をキーにする。対になる api ロールの
// `POST /api/sites/{site}/breakers/{name}/resume`（internal/api/breakers.go）を
// この site 非依存の仕事のブレーカーに対して叩く運用者は、パスに `default` を
// 指定する（`mirakcs:` レジストリに `default` という site が実在する必要がある）。
type DeleteReconcileWorker struct {
	river.WorkerDefaults[DeleteReconcileArgs]
	Pool     *pgxpool.Pool
	MediaDir string

	TrashRetention    time.Duration
	OrphanMTimeGrace  time.Duration
	OrphanAge         time.Duration
	MaxDeletesPerPass int
	MissingAssetAge   time.Duration

	// Webhook は録画ライフサイクル通知用クライアント（M3-11）。nil 可。
	Webhook *webhook.Client
}

// Timeout は River の既定（1 分）より長い上限を与える。
func (w *DeleteReconcileWorker) Timeout(*river.Job[DeleteReconcileArgs]) time.Duration {
	return deleteReconcileTimeout
}

// Work は 1 パス分の削除 reconcile を実行する。
func (w *DeleteReconcileWorker) Work(ctx context.Context, _ *river.Job[DeleteReconcileArgs]) error {
	// site は常に db.DefaultSite（DeleteReconcileWorker の doc コメント参照 ---
	// このジョブはどの束縛サイトの worker が掴んでも同じ 1 つのブレーカーとして
	// 扱う）。
	site := db.DefaultSite
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
	missingAssetAge := w.MissingAssetAge
	if missingAssetAge <= 0 {
		missingAssetAge = defaultMissingAssetAge
	}

	q := sqlcgen.New(w.Pool)

	// パスの先頭でブレーカーの発動状態を DB の真実に合わせ直す
	// （ObserveState のコメント参照。プロセス再起動でゲージが失われるため）。
	tripped, err := breaker.ObserveState(ctx, q, site, breaker.DeleteReconcile)
	if err != nil {
		return fmt.Errorf("observing %s circuit breaker: %w", breaker.DeleteReconcile, err)
	}

	// trash / until_encoded 双方の判定に使う grace_cutoff を先に確定する。
	// pending 経路の再評価（後述）もこの値を使う。
	trashCutoff := time.Now().Add(-trashRetention)

	// deleting のまま止まっていたが、その後の復元などで trash / until_encoded
	// どちらの判定にも該当しなくなった行を active に戻す（issue #105）。
	// ListUnqualifiedDeletingAssets は候補を挙げるだけで書き込まない —— 各行
	// について resolveUnqualifiedDeletingAsset がファイルの現存を確認してから
	// active に戻すか deleted を確定するかを選ぶ（ファイルが既に無いのに
	// active に戻すと「active なのにファイルが無い行」を作ってしまう。これは
	// 案 B [復元時に deleting を同期的に active へ戻す] を却下した理由と同じ
	// 罠で、revert 経路自身がこの窓を作らないようにする）。
	unqualified, err := q.ListUnqualifiedDeletingAssets(ctx, sqlcgen.ListUnqualifiedDeletingAssetsParams{
		GraceCutoff: trashCutoff,
		RowLimit:    deleteReconcileRowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing unqualified deleting assets: %w", err)
	}
	for _, a := range unqualified {
		w.resolveUnqualifiedDeletingAsset(ctx, q, a, trashCutoff)
	}

	// 前パスで deleting のまま止まった行のうち、まだ trash / until_encoded の
	// いずれかの判定に該当するものを最優先で再開する。これは「既に決めた
	// 削除」の再実行であり新規の判断ではないため、サーキットブレーカーの対象外
	// （docs/storage.md §7「どこで落ちても reconcile が拾い直す」）。上の
	// resolveUnqualifiedDeletingAsset で該当しなくなった行は既に active か
	// deleted に決着しているので、ここに現れるのは常に判定を再確認できた
	// ものだけ。
	pending, err := q.ListMediaAssetsPendingDelete(ctx, sqlcgen.ListMediaAssetsPendingDeleteParams{
		GraceCutoff: trashCutoff,
		RowLimit:    deleteReconcileRowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing pending deletes: %w", err)
	}
	// purgedRecordings は、このパスの末尾で MarkPurgedRecordings が purged_at を
	// 押した録画の集合（notifyPurgedRecordings が読む。issue #135）。発火は
	// 削除がすべて終わってから行うので、ブレーカー発動や途中の error で
	// 早期 return する経路も取りこぼさないよう defer で締める —— ただし
	// early return する経路では purgedRecordings は空のままで、それでよい
	// （新しく完全削除が完了した録画が無いということなので、発火対象も無く、
	// 次パスに委ねる）。
	var purgedRecordings []sqlcgen.MarkPurgedRecordingsRow
	defer func() { w.notifyPurgedRecordings(ctx, purgedRecordings, deleteReconcileNotifyBudget) }()

	for _, a := range pending {
		w.deleteMediaAsset(ctx, q, deleteTarget{
			ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes,
		}, "pending")
	}

	// 孤児候補の記録/解除はファイルを消さないので、ブレーカーとは無関係に毎回行う。
	// 同じ 1 回の走査結果（seenOnDisk）を「active なのに実体が無い」検出
	// （逆方向。issue #343）にも使う --- 2 回目の全量ディレクトリ走査を避ける。
	seenOnDisk, err := w.reconcileOrphanCandidates(ctx, q, orphanMTimeGrace)
	if err != nil {
		return fmt.Errorf("reconciling orphan candidates: %w", err)
	}
	missingAssetsSuspected, err := w.reconcileMissingAssets(ctx, q, seenOnDisk)
	if err != nil {
		return fmt.Errorf("reconciling missing assets: %w", err)
	}
	// 疑わしいパス（全損シグネチャ）では記録そのものを見送っているので、
	// 報告（Warn ログ・rokuban_media_assets_missing）も合わせて止める。
	// ここで無条件に呼ぶと、reconcileMissingAssets が今パスの記録を止めた
	// 一方で reportAgedMissingAssets が「前回までに確認済みだった候補」を
	// 毎パス出し続けてしまい、metrics.go / docs/operations/monitoring.md が
	// 約束する「凍結する」が実装のどこにも実在しない記述になる
	// （疑わしい間はゲージを含む報告そのものを止める、という設計判断）。
	if !missingAssetsSuspected {
		if err := w.reportAgedMissingAssets(ctx, q, missingAssetAge); err != nil {
			return fmt.Errorf("reporting aged missing assets: %w", err)
		}
	}

	trashRows, err := q.ListTrashMediaAssetsToDelete(ctx, sqlcgen.ListTrashMediaAssetsToDeleteParams{
		GraceCutoff: trashCutoff,
		RowLimit:    deleteReconcileRowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing trash assets past retention: %w", err)
	}

	// 原本を入力とする active な encode/thumbnail ジョブの有無はここでは見ない
	// （旧条件 3。docs/storage/retention.md「削除可否の述語に名前を与える」
	// 直後の判断根拠参照）。until_encoded_deletable_originals（条件 2）が
	// desired な派生物の完備を要求するため、出力未コミットのジョブは既に
	// 条件 2 が止め、出力コミット済みのジョブは各ワーカーの冒頭の冪等チェックが
	// 原本を開かずに skip する。
	untilEncodedRows, err := q.ListUntilEncodedOriginalsToDelete(ctx, deleteReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing until_encoded originals: %w", err)
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
		w.deleteMediaAsset(ctx, q, deleteTarget{
			ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes,
		}, "trash")
	}
	for _, a := range untilEncodedRows {
		w.deleteMediaAsset(ctx, q, deleteTarget{
			ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes,
		}, "until_encoded")
	}
	for _, relPath := range agedOrphans {
		w.deleteOrphanFile(q, relPath)
	}

	// パスの末尾で「完全削除が完了した」という不可逆な事実を確定する
	// （issue #135、MarkPurgedRecordings のコメント参照）。ここより前の
	// pending / trash / until_encoded の 3 ループがすべて物理 unlink を終えた
	// 後でなければならない —— 先頭で押すと同じパスで消したアセットが
	// 反映されず、判定が 1 パス遅れる。
	purged, err := q.MarkPurgedRecordings(ctx, trashCutoff)
	if err != nil {
		return fmt.Errorf("marking purged recordings: %w", err)
	}
	purgedRecordings = purged

	metrics.DeleteReconcileLastPass.SetToCurrentTime()
	return nil
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

	// 既に deleting なら 0 行更新（それでよい。pending 経路はこの UPDATE を
	// スキップしても問題ない冪等な no-op）。active からの遷移だけを記録する。
	if _, err := q.MarkMediaAssetDeleting(ctx, t.ID); err != nil {
		log.Error("delete_reconcile: marking asset deleting", "err", err)
		return
	}

	path, err := mediapath.Resolve(w.MediaDir, t.RelPath)
	if err != nil {
		log.Error("delete_reconcile: rejecting rel_path outside the media directory", "err", err)
		return
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// unlink 失敗。行は deleting のまま残し、次パスで再試行する。
		log.Error("delete_reconcile: removing file", "err", err)
		return
	}

	if _, err := q.MarkMediaAssetDeleted(ctx, t.ID); err != nil {
		log.Error("delete_reconcile: marking asset deleted", "err", err)
		return
	}

	metrics.DeleteReconcileDeleted.WithLabelValues(source).Inc()
	metrics.DeleteReconcileBytes.WithLabelValues(source).Add(float64(t.SizeBytes))
	log.Info("delete_reconcile: deleted asset")
}

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

// notifyPurgedRecordings は「録画そのものが消えた」録画について
// recording.deleted を発火する（M3-11、issue #135）。失敗はログのみ
// （本処理を止めない）。
//
// 発火対象は purged（引数の purged）そのものであり、ここで改めて「録画が
// 消えたか」を計算し直すことはしない --- 通知の一瞬だけ計算して結果を
// 捨てる形だと、アセットを 1 行も持たない録画で「消した」と「元から無い」を
// 区別できず発火し損なう。その計算は MarkPurgedRecordings の WHERE で
// 1 回だけ行われ、結果は purged_at として永続化済みである。
//
// purged は MarkPurgedRecordings が WHERE purged_at IS NULL で選んだ集合
// なので、同じ録画で二度発火することはない（次パスでは同じ行が候補に
// 上がらない）。
func (w *DeleteReconcileWorker) notifyPurgedRecordings(ctx context.Context, purged []sqlcgen.MarkPurgedRecordingsRow, budget time.Duration) {
	if w.Webhook == nil || len(purged) == 0 {
		return
	}
	// POST だけに時間上限を与える（DB 読みは親 ctx のまま）。
	notifyCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for i, r := range purged {
		// 捨てた件数は必ずログに残す（黙って打ち切ると「全件通知した」と読めてしまう）。
		if err := ctx.Err(); err != nil {
			slog.Warn("delete_reconcile: pass context done, dropping notifications",
				"dropped", len(purged)-i, "err", err)
			return
		}
		if notifyCtx.Err() != nil {
			slog.Warn("delete_reconcile: webhook budget exhausted, dropping notifications",
				"budget", budget, "dropped", len(purged)-i)
			return
		}
		ev := webhook.Event{
			Type:        webhook.EventRecordingDeleted,
			RecordingID: r.ID,
			Site:        r.Site,
			Title:       r.Title,
			Status:      "deleted",
		}
		if err := w.Webhook.Notify(notifyCtx, ev); err != nil {
			slog.Error("webhook notify failed",
				"type", webhook.EventRecordingDeleted, "recording_id", r.ID, "err", err)
		}
	}
}

// reconcileOrphanCandidates は MediaDir を走査し、media_assets のどの行からも
// 参照されていないファイルを孤児候補として orphan_files に記録する。
// mtime が新しいファイル（OrphanMTimeGrace 以内）は候補にしない。
// 既に孤児でなくなった（登録された、またはファイルが消えた）行は掃除する。
// ファイルは一切消さない（記録のみ）。
//
// 戻り値はこの 1 回の走査で実際にディスク上で観測した全 rel_path の集合
// （孤児かどうかを問わない）。reconcileMissingAssets が同じ走査結果を使って
// 逆方向（active なのに実体が無い行）を検出するため（issue #343。2 回目の
// 全量ディレクトリ走査を避ける）。
func (w *DeleteReconcileWorker) reconcileOrphanCandidates(ctx context.Context, q *sqlcgen.Queries, mtimeGrace time.Duration) (map[string]struct{}, error) {
	known, err := q.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing known rel paths: %w", err)
	}
	knownSet := make(map[string]struct{}, len(known))
	for _, k := range known {
		knownSet[k] = struct{}{}
	}

	mtimeCutoff := time.Now().Add(-mtimeGrace)
	seenOnDisk := make(map[string]struct{})
	candidates := make(map[string]struct{})
	if err := walkMediaFiles(w.MediaDir, func(relPath string, info fs.FileInfo) {
		seenOnDisk[relPath] = struct{}{}
		if _, ok := knownSet[relPath]; ok {
			return
		}
		if info.ModTime().After(mtimeCutoff) {
			return
		}
		candidates[relPath] = struct{}{}
	}); err != nil {
		return nil, fmt.Errorf("walking media dir: %w", err)
	}

	for relPath := range candidates {
		if err := q.UpsertOrphanFile(ctx, relPath); err != nil {
			return nil, fmt.Errorf("recording orphan candidate %q: %w", relPath, err)
		}
	}

	existing, err := q.ListAllOrphanFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing orphan files: %w", err)
	}
	for _, o := range existing {
		if _, stillCandidate := candidates[o.RelPath]; stillCandidate {
			continue
		}
		// もう孤児候補でない（media_assets に登録された、mtime が新しくなった
		// はずはないが再走査で見えなくなった＝ファイルが消えた等）。掃除する。
		if err := q.DeleteOrphanFile(ctx, o.RelPath); err != nil {
			return nil, fmt.Errorf("clearing stale orphan record %q: %w", o.RelPath, err)
		}
	}
	return seenOnDisk, nil
}

// reconcileMissingAssets は orphan 検出と同じ 1 回の走査結果（seenOnDisk）を
// 使い、state='active' な media_assets のうちディスク上で観測されなかった
// 行を missing_media_assets に記録する（issue #343、孤児回収の逆方向）。
// ファイルを消さず、media_assets も一切書き換えない（記録のみ）。
//
// 走査を逆向きに再利用すると walkMediaFiles の除外（catalog.Subdir の
// SkipDir）の誤りの向きが反転する: 孤児方向では除外されたパスは「孤児候補に
// しない」＝削除しない側に倒れるが、逆方向では除外されたパスの資産が
// 恒久的に「実体無し」と誤報される（15 分ごとの Warn + ゲージが下がらない。
// 削除はしないので被害は騒音のみ）。今日は catalog/ がトップレベルの予約
// ディレクトリなので該当する rel_path は存在しないが（docs/storage/contract.md
// §5）、walkMediaFiles に除外を足すときはこちら側の誤報を先に確認する。
// 除外と同型の穴が「走査が降りないディレクトリ」の側にもある --- filepath.Walk
// は symlink を辿らないので、ディレクトリ成分が symlink の構成では配下の資産が
// 同じ形で誤報される（walkMediaFiles の doc コメント。未検証）。
//
// reconcileOrphanCandidates は走査より前に ListAllMediaAssetRelPaths を読むが、
// ここでの ListActiveMediaAssets は走査より後に読む（呼び出し順序どおり）。
// そのため走査の途中でコミットされた資産は、この 1 パスでは一時的に
// 「実体無し」の偽候補になりうる —— この窓も MissingAssetAge のエイジングが
// 埋める（次パスでファイルが観測されれば候補は掃除され、確認済みとして
// 報告される前に消える）。
//
// マウントが落ちている・空マウントのときに全 active 行を「実体無し」と
// 報告して騒がないよう、seenOnDisk が空（この走査で 1 件もファイルを
// 観測できなかった）のに active な行が存在するケースを形で検知して丸ごと
// 見送る（reconciler の全損シグネチャ breaker.ReconcileTotalLoss と同じ
// 考え方 --- 件数の閾値ではなく形で見る。ただしここは削除を止める
// ブレーカーではなく単なる観測の記録なので、ラッチは持たず今パスの記録を
// 見送るだけでよい）。既存の missing_media_assets 行にも一切触れない ---
// 前回までの確認済み状態をこの疑わしいパスの結果で上書きしないため。
//
// 戻り値の bool は今パスが疑わしいパス（全損シグネチャ発動）だったかを
// 呼び出し元（Work）に伝える。呼び出し元はこれを見て reportAgedMissingAssets
// の呼び出し自体を止める --- 記録を見送りながら報告（Warn ログ・
// rokuban_media_assets_missing ゲージ）だけ続けると、metrics.go /
// docs/operations/monitoring.md が約束する「疑わしい間はゲージが凍結する」が
// 実装のどこにも存在しない記述になる。
//
// # ListActiveMediaAssets に deleteReconcileRowLimit を掛けない理由
//
// このファイルの他のソースは全部 deleteReconcileRowLimit で括っているが、
// ここだけは全件を読む。**この判定は差集合なので、入力を切ると答えが変わる**
// --- 窓の外に落ちた active 行はこのパスの candidates に入らず、下の掃除
// ループが「もう候補でない」と見なして既存の missing_media_assets 行を消して
// しまう。first_seen が毎パス失われるので、active が窓を超えている系では
// エイジングが永久に完了せず何も報告されない（他のソースは「拾えた分だけ
// 消す」で正しさが保たれるので窓で足りる。差集合ではないため）。
//
// 作業量の上限は候補数（実体が無い行の数）であって active 全件ではない ---
// 健全な系では候補 0 件で、増える I/O はこの SELECT 1 回だけ。部分マウント
// 障害（walkMediaFiles の doc コメント）では候補が active 全件近くまで伸び、
// 15 分ごとに同じ件数の UPSERT が削除本体と同じジョブの中で走る。それが
// 問題になるなら直し方は入力を切ることではなく UPSERT を 1 文にまとめること。
func (w *DeleteReconcileWorker) reconcileMissingAssets(ctx context.Context, q *sqlcgen.Queries, seenOnDisk map[string]struct{}) (bool, error) {
	active, err := q.ListActiveMediaAssets(ctx)
	if err != nil {
		return false, fmt.Errorf("listing active media assets: %w", err)
	}
	if len(active) == 0 {
		// active が 1 件も無いパスでは下の掃除ループに入らないので、残っている
		// missing_media_assets 行はこのパスでは掃除されない。報告側
		// （ListAgedMissingMediaAssets）が state='active' で弾くので害は無く、
		// active が 1 件でもあるパスで回収される（意図的）。
		return false, nil
	}
	if len(seenOnDisk) == 0 {
		slog.Warn("delete_reconcile: filesystem walk observed zero files while active media_assets rows exist; suspecting a storage mount failure, skipping missing-asset check for this pass",
			"active_assets", len(active))
		metrics.MissingAssetScanSuspectedStorageFailure.Inc()
		return true, nil
	}

	candidates := make(map[int64]struct{})
	for _, a := range active {
		if _, ok := seenOnDisk[a.RelPath]; ok {
			continue
		}
		candidates[a.ID] = struct{}{}
		if err := q.UpsertMissingMediaAsset(ctx, a.ID); err != nil {
			return false, fmt.Errorf("recording missing-asset candidate %d: %w", a.ID, err)
		}
	}

	existingIDs, err := q.ListAllMissingMediaAssetIDs(ctx)
	if err != nil {
		return false, fmt.Errorf("listing missing-asset candidates: %w", err)
	}
	for _, id := range existingIDs {
		if _, stillCandidate := candidates[id]; stillCandidate {
			continue
		}
		// もう実体無し候補でない（ファイルが見つかった、またはこの資産自体が
		// もう active でなくなった等）。掃除する。
		if err := q.DeleteMissingMediaAsset(ctx, id); err != nil {
			return false, fmt.Errorf("clearing stale missing-asset record %d: %w", id, err)
		}
	}
	return false, nil
}

// reportAgedMissingAssets は missing_media_assets のうち first_seen が age を
// 超えて連続して記録されている（単発の走査揺れではない）行を、確認済みの
// 異常として Warn ログとメトリクスに出す（issue #343）。media_assets /
// missing_media_assets のどちらも書き換えない --- 自動削除は行わない
// （「ファイルが無い」は削除の必要条件であって十分条件ではない。
// docs/storage/retention.md §7「孤児回収の逆」）。
//
// 個別 Warn ログは missingAssetLogBudget 件で打ち切り、超過分は件数だけを
// 載せた 1 行（属性 logged / and_more）にまとめる --- 対象が解消するまで
// defaultDeleteReconcileInterval ごとに同じ全件が再送され続けるため
// （件数自体は Reset 後の MediaAssetsMissing ゲージが持つので、ログは同定
// だけを担えばよい）。
func (w *DeleteReconcileWorker) reportAgedMissingAssets(ctx context.Context, q *sqlcgen.Queries, age time.Duration) error {
	aged, err := q.ListAgedMissingMediaAssets(ctx, time.Now().Add(-age))
	if err != nil {
		return fmt.Errorf("listing aged missing-asset candidates: %w", err)
	}

	counts := make(map[string]int, 3)
	for i, a := range aged {
		counts[a.Kind]++
		if i < missingAssetLogBudget {
			slog.Warn("delete_reconcile: active media asset has no file on disk",
				"media_asset_id", a.ID, "recording_id", a.RecordingID, "rel_path", a.RelPath,
				"kind", a.Kind, "first_seen", a.FirstSeen)
		}
	}
	if len(aged) > missingAssetLogBudget {
		slog.Warn("delete_reconcile: suppressing further missing-asset log lines this pass",
			"logged", missingAssetLogBudget, "and_more", len(aged)-missingAssetLogBudget)
	}

	// EncodeReconcileUnsatisfiable と同じパターン: Reset してから現在の
	// パスで見た kind だけ Set する。該当 0 件の kind はラベルの系列自体が
	// 消える（0 を出さない）。
	metrics.MediaAssetsMissing.Reset()
	for kind, n := range counts {
		metrics.MediaAssetsMissing.WithLabelValues(kind).Set(float64(n))
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
//
// 除外を足すときは reconcileMissingAssets の doc コメントを読む --- この結果は
// 孤児方向と実体無し方向の両方が使い、除外の誤りの向きが二者で反対になる。
//
// filepath.Walk は symlink を辿らないので、mediaDir 配下のディレクトリ成分が
// symlink になっている構成（`media/sites -> /mnt/nas/sites` のような合成）では
// そのサブツリーに降りない。ingest / streamer は字句解決（mediapath.Resolve）
// なのでそこへ書けて読めるが、走査からは見えない --- 除外（SkipDir）と同型の
// 穴で、こちらは設定だけで作れる。孤児方向では安全側（消さない）に倒れ、
// 実体無し方向では配下の資産が恒久的に誤報される（未検証。symlink を張った
// 構成でのテストは無い）。symlink を辿る形にはしていない: 循環と、
// mediaDir の外を指す symlink の扱い（rel_path が mediaDir の外のファイルを
// 指すことになる）を先に決める必要があり、この検出器の範囲を超える。
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
