package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/webhook"
)

const (
	// defaultTrashRetention はごみ箱（recordings.deleted_at）の既定猶予（30 日）。
	defaultTrashRetention = 30 * 24 * time.Hour

	// defaultOrphanMTimeGrace は孤児候補にするまでの既定 mtime 猶予（7 日）。
	defaultOrphanMTimeGrace = 7 * 24 * time.Hour

	// defaultOrphanAge は孤児候補が確認済みとして報告されるまでの既定エイジング期間（14 日）。
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
	// エイジング済み候補は解消するまで定期パス間隔（既定
	// 15 分 = 1 日 96 パス）ごとに全件が再送されるので、上限が無ければ
	// 「候補数 × 96」行/日になる（劣化したマウントを放置すると候補数は
	// active な media_assets 全件まで伸びうる。実測はしていない算術）。
	// 超過分は件数だけを 1 行にまとめる。deleteReconcileNotifyBudget が
	// 時間の予算なのに対し、こちらは件数の予算（別物）。
	missingAssetLogBudget = 20

	// orphanFileLogBudget は 1 パスで canonical 孤児を個別に Warn する件数の上限。
	// 孤児候補は DB 全損直後に media_dir の全ファイルまで増えうるため、超過分は
	// ゲージと件数だけの 1 行にまとめる。
	orphanFileLogBudget = 20

	// deleteReconcileRowLimit はソースごとに 1 パスで拾う行数の上限。
	// 際限なく積み上げてタイムアウトするのを避けるための安全弁で、
	// サーキットブレーカーの閾値（既定 100）より十分大きく取る。
	deleteReconcileRowLimit = 5000

	// deleteReconcileNotifyBudget は 1 パスの webhook 発火全体に与える時間上限。
	// 発火は削除がすべて終わった後に行うので削除の進捗を遅らせることはないが、
	// 通知先がハングすると 1 件あたり timeout × 2（既定 10 秒）を数十件分積み上げ、
	// ジョブの締切予算を食い潰してパスごと失敗させうる。webhook より
	// 本処理を優先する（M3-11）ため、超過分は捨てて件数をログに残す。
	deleteReconcileNotifyBudget = 2 * time.Minute

	// deleteOrphanCleanupTimeout は ctx 取消後も削除の事実を DB に記録するための
	// 書き込み（DeleteOrphanFile）に与える上限。
	deleteOrphanCleanupTimeout = 10 * time.Second
)

// deleteTarget は物理削除 1 件分の対象。pending / trash / until_encoded /
// removed_encoded の sqlc 行型は同じ列を返すので、ここで 1 つに寄せる（引数を平たく
// 並べると同型のスカラが増えて取り違えがコンパイルで検出できない）。
type deleteTarget struct {
	ID          int64
	RecordingID int64
	RelPath     string
	SizeBytes   int64
	Kind        string
}

// DeleteReconcileWorker は物理 unlink に至る 4 ソース（ごみ箱 / until_encoded /
// ユーザーが外したエンコード版 / 孤児）を 1 本の reconcile ループに統一する queue worker（docs/storage.md §7）。
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
// # サーキットブレーカーのキーは site を持たない（Site フィールドを持たない理由）
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
// したがって `Deps.Site` を一切参照せず、site 列には空文字列
// （breaker.IsSiteless のコメント参照。named const にしていない理由も同所）を
// 書く。かつては db.DefaultSite（"default"）を書いていたが、`mirakcs:`
// レジストリに `default` という名前の site が実在しない構成（東京・高松の
// 2 site 化以降はこれが既定）では、site スコープの
// `POST /api/sites/{site}/breakers/{name}/resume` から `default` を指定しても
// レジストリに無いので 400、登録済みの site を指定してもその site には行が
// 無いので 404 になり、`GET /api/breakers` に見えているのに一切解除できなかった
// （issue #450）。db.DefaultSite は「site 未指定なら設定済みの唯一の site に
// 解決する」という意味の値であり、「この資源は構造的に site を持たない」とは
// 別の概念なので、混同しないよう使うのをやめた。site を持たないブレーカーは
// site 無しの `POST /api/breakers/{name}/resume`
// （internal/api/breakers.go の ResumeSitelessCircuitBreaker）で再開する。
//
// 既存の `(site='default', name='delete_reconcile')` 行は寄せていない ---
// この時点で運用中の DB がまだ無い（マイグレーションはベースラインへの
// squash 済みで、#52 の EPGStation 並走もまだ始まっていない）ため、移行対象の
// 実データが存在しない。**この行が万一残っていれば手で DELETE すること**:
// worker は site 列が空文字列の行しか見ないので発動中と気付かず、人間の確認
// を強制するラッチを飛ばして bulk unlink を再開してしまう。しかもその行は
// どちらの resume エンドポイントからも解除できない
// （site スコープは IsSiteless で 400、site 無しは対象行が無く 404）。
type DeleteReconcileWorker struct {
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

// Work は 1 パス分の削除 reconcile を実行する。
func (w *DeleteReconcileWorker) Work(ctx context.Context, _ jobs.DeleteReconcileArgs) error {
	// site は常に空文字列（DeleteReconcileWorker の doc コメント参照 ---
	// このジョブはどの束縛サイトの worker が掴んでも同じ 1 つのブレーカーとして
	// 扱う。breaker.IsSiteless(breaker.DeleteReconcile) が true）。
	site := ""
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
	tripped, err := w.observeDeleteReconcile(ctx, q, site)
	if err != nil {
		return err
	}
	// trash / until_encoded 双方の判定に使う grace_cutoff を先に確定する。
	// pending 経路の再評価（後述）もこの値を使う。
	trashCutoff := time.Now().Add(-trashRetention)
	if err := w.restoreUnqualifiedDeletingAssets(ctx, q, trashCutoff); err != nil {
		return err
	}
	// pending の再開はブレーカー対象外（listPendingDeleteAssets のコメント
	// 参照。既に決めた削除の再実行であり新規の判断ではない）。
	pending, err := listPendingDeleteAssets(ctx, q, trashCutoff)
	if err != nil {
		return err
	}
	// purgedRecordings は、このパスの末尾で MarkPurgedRecordings が purged_at を
	// 押した録画の集合（notifyPurgedRecordings が読む。issue #135）。発火は
	// 削除がすべて終わってから行うので、ブレーカー発動や途中の error で
	// 早期 return する経路も取りこぼさないよう defer で締める --- ただし
	// early return する経路では purgedRecordings は空のままで、それでよい
	// （新しく完全削除が完了した録画が無いということなので、発火対象も無く、
	// 次パスに委ねる）。
	var purgedRecordings []sqlcgen.MarkPurgedRecordingsRow
	defer func() { w.notifyPurgedRecordings(ctx, purgedRecordings, deleteReconcileNotifyBudget) }()
	for _, a := range pending {
		w.deleteMediaAsset(ctx, q, deleteTarget{
			ID: a.ID, RecordingID: a.RecordingID, RelPath: a.RelPath, SizeBytes: a.SizeBytes, Kind: a.Kind,
		}, "pending")
	}

	if err := w.reconcileDeleteObservations(ctx, q, orphanMTimeGrace, orphanAge, missingAssetAge); err != nil {
		return err
	}
	candidates, err := collectDeleteCandidates(ctx, q, trashCutoff)
	if err != nil {
		return err
	}
	candidates.agedOrphans, err = w.agedUnpublishedOrphans(ctx, q, orphanAge, orphanMTimeGrace)
	if err != nil {
		return fmt.Errorf("verifying aged unpublished files: %w", err)
	}
	afterDeleteCandidatesCollected()
	stop, err := w.applyDeleteCircuitBreaker(ctx, q, site, tripped, candidates.total(), maxPerPass)
	if err != nil {
		return err
	}
	if stop {
		metrics.DeleteReconcileLastPass.SetToCurrentTime()
		return nil
	}
	w.deleteCandidates(ctx, q, candidates, orphanMTimeGrace)

	// パスの末尾で「完全削除が完了した」という不可逆な事実を確定する
	// （issue #135、MarkPurgedRecordings のコメント参照）。ここより前の
	// pending ループと deleteCandidates（trash / until_encoded / 孤児）が
	// すべて物理 unlink を終えた後でなければならない —— 先頭で押すと
	// 同じパスで消したアセットが反映されず、判定が 1 パス遅れる。
	purged, err := q.MarkPurgedRecordings(ctx, trashCutoff)
	if err != nil {
		return fmt.Errorf("marking purged recordings: %w", err)
	}
	purgedRecordings = purged

	metrics.DeleteReconcileLastPass.SetToCurrentTime()
	return nil
}

// afterDeleteCandidatesCollected は候補収集の後・ブレーカー判定と削除の前に置くテスト用の同期点。
// 本番では何もしない。
var afterDeleteCandidatesCollected = func() {}

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
