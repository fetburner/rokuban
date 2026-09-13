package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
)

const (
	// defaultThumbnailReconcileInterval は thumbnail の desired−observed 定期パスの
	// 既定間隔。ingest 完了時のヒント投入を補うバックストップとして encode
	// reconcile と揃える。
	defaultThumbnailReconcileInterval = 15 * time.Minute

	// thumbnailReconcileTimeout は 1 パス全体の上限。thumbnail の抽出自体は
	// ThumbnailWorker が行い、このパスは候補の読み取りと River への投入だけを行う。
	thumbnailReconcileTimeout = 5 * time.Minute

	// thumbnailReconcileRowLimit は 1 パスで拾う録画候補の既定上限。
	thumbnailReconcileRowLimit = 1000
)

// ThumbnailReconcileWorker は desired（active な original）− observed（active な
// thumbnail）の差分を定期的に埋める River ワーカー。
//
// ingest 完了後の thumbnail ヒント投入と、明示的な EnqueueMissingThumbnails は
// どちらもベストエフォートの経路である。ヒント投入が失敗した後に mirakc 側の
// record が削除されると record_sweep からも再投入できないため、このワーカーは
// DB の状態を真実として差分を拾い直す。
//
// missing_media_assets に記録された原本は、delete_reconcile が実体無しを確認した
// 既知の恒久失敗なので定期パスの候補から除く。ファイルが復旧してマーカーが
// 消えれば、次のパスで自動的に候補へ戻る。ffprobe / ffmpeg の実行失敗のように
// DB だけでは恒久性を判定できない失敗は、level-triggered な再投入と候補窓の
// 回転で扱う。
//
// thumbnail_reconcile は ThumbnailWorker と同じ thumbnail キューを使う。River の
// キュー単位 MaxWorkers はジョブ種を区別しないため、thumbnail の抽出中はこの
// パスも待つが、次の周期に同じ desired−observed を再確認すればよい。
type ThumbnailReconcileWorker struct {
	river.WorkerDefaults[jobs.ThumbnailReconcileArgs]
	Pool     *pgxpool.Pool
	RowLimit int32

	// resumeAfter は次のパスが候補を探し始める位置（この recording_id より
	// 大きい候補から見る）。候補が RowLimit 件以上あるときだけ使い、末尾まで
	// 到達したパスで 0 に戻す。プロセスローカルで永続化しない。
	resumeAfter atomic.Int64
}

// Timeout は River の既定（1 分）より長い上限を与える。thumbnail の生成時間は
// 含まず、候補の読み取りと投入だけに使う。
func (w *ThumbnailReconcileWorker) Timeout(*river.Job[jobs.ThumbnailReconcileArgs]) time.Duration {
	return thumbnailReconcileTimeout
}

// Work は 1 パス分の thumbnail reconcile を実行する。
//
// 候補は recording_id 単位で keyset pagination する。RowLimit ちょうどまで返った
// ときは最後に見た recording_id を保存して次のパスで窓を進め、少ないときは
// 先頭へ戻る。これにより、thumbnail の抽出が恒久的に失敗する録画が先頭に
// 残っても後続の候補を無期限に隠さない。
func (w *ThumbnailReconcileWorker) Work(ctx context.Context, _ *river.Job[jobs.ThumbnailReconcileArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("thumbnail reconcile: getting river client: %w", err)
	}

	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = thumbnailReconcileRowLimit
	}

	after := w.resumeAfter.Load()
	rows, err := sqlcgen.New(w.Pool).ListMissingThumbnailRecordings(ctx, sqlcgen.ListMissingThumbnailRecordingsParams{
		AfterRecordingID: after,
		RowLimit:         rowLimit,
	})
	if err != nil {
		// DB 取得に失敗したパスは完走していないので、再開位置を変えない。
		return fmt.Errorf("listing missing thumbnail recordings: %w", err)
	}

	failed := 0
	for _, recordingID := range rows {
		if _, err := client.Insert(ctx, jobs.ThumbnailJobArgs{RecordingID: recordingID}, nil); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue missing thumbnail",
				"recording_id", recordingID, "err", err)
		}
	}

	metrics.ThumbnailReconcileCandidates.Set(float64(len(rows)))
	metrics.ThumbnailReconcileLastPass.SetToCurrentTime()

	var resumeAfter int64
	if int32(len(rows)) >= rowLimit {
		resumeAfter = rows[len(rows)-1]
		slog.Warn("thumbnail_reconcile: candidate window is full; the next pass resumes from resume_after",
			"row_limit", rowLimit, "last_recording_id", resumeAfter, "resume_after", resumeAfter)
	}
	w.resumeAfter.Store(resumeAfter)

	if len(rows) > 0 || failed > 0 {
		slog.Info("thumbnail_reconcile: pass complete",
			"candidates", len(rows), "failed", failed, "row_limit", rowLimit, "resume_after", resumeAfter)
	}
	return nil
}
