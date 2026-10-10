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

// ThumbnailReconcileWorker は missing thumbnail / seek_tiles と、チャプターにより
// 再選択が必要な thumbnail を定期パスで見つける River ワーカー。
//
// ingest 後の対象限定パスと定期全件パスは同じ候補クエリを使う。対象限定パスが
// 投入されなかった場合も、定期パスは DB の状態を真実として差分を拾い直す。
//
// missing_media_assets に記録された原本は、delete_reconcile が実体無しを確認した
// 既知の恒久失敗なので定期パスの候補から除く。ファイルが復旧してマーカーが
// 消えれば、次のパスで自動的に候補へ戻る。ffprobe / ffmpeg の実行失敗のように
// DB だけでは恒久性を判定できない失敗は、level-triggered な再投入と候補窓の
// 回転で扱う。
//
// thumbnail_reconcile は ThumbnailWorker / SeekTilesWorker と同じ thumbnail
// キューを使う。River のキュー単位 MaxWorkers はジョブ種を区別しないため、
// thumbnail の抽出中はこのパスも待つが、次の周期に同じ desired−observed を
// 再確認すればよい。
//
// missing thumbnail と再選択候補はどちらも poster の一部として同じキューへ積む。
// **seek_tiles も同じパスが埋める**が、候補集合は別なので poster の窓と seek_tiles
// の窓は独立に巡回する。どちらかだけが上限に張り付いたとき、共通カーソルで他方の
// 候補を飛ばさないためである。
type ThumbnailReconcileWorker struct {
	river.WorkerDefaults[jobs.ThumbnailReconcileArgs]
	Pool     *pgxpool.Pool
	RowLimit int32

	// 3 つの resume 位置は次のパスが候補を探し始める位置（この recording_id より
	// 大きい候補から見る）。各候補窓が RowLimit 件以上あるときだけ使い、末尾まで
	// 到達した窓は 0 に戻す。プロセスローカルで永続化しない。
	resumeAfter          atomic.Int64
	reselectResumeAfter  atomic.Int64
	seekTilesResumeAfter atomic.Int64
}

// Timeout は River の既定（1 分）より長い上限を与える。thumbnail の生成時間は
// 含まず、候補の読み取りと投入だけに使う。
func (w *ThumbnailReconcileWorker) Timeout(*river.Job[jobs.ThumbnailReconcileArgs]) time.Duration {
	return thumbnailReconcileTimeout
}

// Work は thumbnail reconcile を全件または指定録画について実行する。
//
// missing thumbnail、thumbnail の再選択、seek_tiles は recording_id 単位で独立に
// keyset pagination する。各窓が RowLimit ちょうどまで返ったときは最後に見た
// recording_id を保存して次のパスで進み、少ないときは先頭へ戻る。恒久的に失敗する
// 録画が先頭に残っても後続候補を無期限に隠さない。
func (w *ThumbnailReconcileWorker) Work(ctx context.Context, job *river.Job[jobs.ThumbnailReconcileArgs]) error {
	recordingID := job.Args.RecordingID
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("thumbnail reconcile: getting river client: %w", err)
	}

	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = thumbnailReconcileRowLimit
	}

	after := int64(0)
	reselectAfter := int64(0)
	var targetID *int64
	if recordingID == 0 {
		after = w.resumeAfter.Load()
		reselectAfter = w.reselectResumeAfter.Load()
	} else {
		targetID = &recordingID
	}
	q := sqlcgen.New(w.Pool)
	rows, err := q.ListMissingThumbnailRecordings(ctx, sqlcgen.ListMissingThumbnailRecordingsParams{
		AfterRecordingID: after,
		RecordingID:      targetID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		// DB 取得に失敗したパスは完走していないので、再開位置を変えない。
		return fmt.Errorf("listing missing thumbnail recordings: %w", err)
	}

	reselectRows, err := q.ListThumbnailReselectCandidates(ctx, sqlcgen.ListThumbnailReselectCandidatesParams{
		AfterRecordingID: reselectAfter,
		RecordingID:      targetID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing thumbnail reselection candidates: %w", err)
	}

	failed := 0
	for _, recordingID := range rows {
		if _, err := client.Insert(ctx, jobs.ThumbnailJobArgs{RecordingID: recordingID}, nil); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue missing thumbnail",
				"recording_id", recordingID, "err", err)
		}
	}
	for _, row := range reselectRows {
		timeline, hasTimeline, err := thumbnailTimeline(row)
		if err != nil {
			failed++
			slog.Error("thumbnail_reconcile: could not derive candidate timeline",
				"recording_id", row.RecordingID, "err", err)
			continue
		}
		if !hasTimeline {
			continue
		}
		inputs, err := thumbnailInputsOf(row)
		if err != nil {
			failed++
			slog.Error("thumbnail_reconcile: could not decode candidate inputs",
				"recording_id", row.RecordingID, "err", err)
			continue
		}
		if !thumbnailNeedsReselect(row.SeekMs, inputs, timeline) {
			continue
		}
		if _, err := client.Insert(ctx, jobs.ThumbnailJobArgs{RecordingID: row.RecordingID}, nil); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue reselection",
				"recording_id", row.RecordingID, "err", err)
		}
	}

	seekTilesFailed := w.enqueueMissingSeekTiles(ctx, client, rowLimit, targetID)
	failed += seekTilesFailed

	if recordingID == 0 {
		metrics.ThumbnailReconcileCandidates.Set(float64(len(rows) + len(reselectRows)))
		metrics.ThumbnailReconcileLastPass.SetToCurrentTime()

		var resumeAfter int64
		if int32(len(rows)) >= rowLimit {
			resumeAfter = rows[len(rows)-1]
			slog.Warn("thumbnail_reconcile: candidate window is full; the next pass resumes from resume_after",
				"row_limit", rowLimit, "last_recording_id", resumeAfter, "resume_after", resumeAfter)
		}
		w.resumeAfter.Store(resumeAfter)

		var reselectResumeAfter int64
		if int32(len(reselectRows)) >= rowLimit {
			reselectResumeAfter = reselectRows[len(reselectRows)-1].RecordingID
			slog.Warn("thumbnail_reconcile: reselection candidate window is full; the next pass resumes",
				"row_limit", rowLimit, "last_recording_id", reselectResumeAfter)
		}
		w.reselectResumeAfter.Store(reselectResumeAfter)

		if len(rows) > 0 || len(reselectRows) > 0 || failed > 0 {
			slog.Info("thumbnail_reconcile: pass complete",
				"missing_candidates", len(rows), "reselection_candidates", len(reselectRows),
				"failed", failed, "row_limit", rowLimit,
				"resume_after", resumeAfter, "reselection_resume_after", reselectResumeAfter)
		}
	} else if len(rows) > 0 || len(reselectRows) > 0 || failed > 0 {
		slog.Info("thumbnail_reconcile: targeted pass complete",
			"recording_id", recordingID, "missing_candidates", len(rows),
			"reselection_candidates", len(reselectRows), "failed", failed)
	}
	return nil
}

// enqueueMissingSeekTiles は seek_tiles の desired−observed ギャップを埋める。
// thumbnail と同じ窓の形（keyset pagination）だが、全件パスでは再開位置を独立に
// 持つ。戻り値は投入に失敗した件数。
func (w *ThumbnailReconcileWorker) enqueueMissingSeekTiles(ctx context.Context, client *river.Client[pgx5.Tx], rowLimit int32, targetID *int64) int {
	after := int64(0)
	if targetID == nil {
		after = w.seekTilesResumeAfter.Load()
	}
	rows, err := sqlcgen.New(w.Pool).ListMissingSeekTilesRecordings(ctx, sqlcgen.ListMissingSeekTilesRecordingsParams{
		AfterRecordingID: after,
		RecordingID:      targetID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		// DB 取得に失敗したパスは完走していないので、再開位置を変えない。
		slog.Error("thumbnail_reconcile: listing missing seek tiles failed", "err", err)
		return 1
	}

	failed := 0
	for _, recordingID := range rows {
		if _, err := client.Insert(ctx, jobs.SeekTilesJobArgs{RecordingID: recordingID}, nil); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue missing seek tiles",
				"recording_id", recordingID, "err", err)
		}
	}

	if targetID == nil {
		var resumeAfter int64
		if int32(len(rows)) >= rowLimit {
			resumeAfter = rows[len(rows)-1]
			slog.Warn("thumbnail_reconcile: seek tiles candidate window is full; the next pass resumes from resume_after",
				"row_limit", rowLimit, "last_recording_id", resumeAfter)
		}
		w.seekTilesResumeAfter.Store(resumeAfter)
		if len(rows) > 0 || failed > 0 {
			slog.Info("thumbnail_reconcile: seek tiles pass complete",
				"candidates", len(rows), "failed", failed, "row_limit", rowLimit, "resume_after", resumeAfter)
		}
	} else if len(rows) > 0 || failed > 0 {
		slog.Info("thumbnail_reconcile: targeted seek tiles pass complete",
			"recording_id", *targetID, "candidates", len(rows), "failed", failed)
	}
	return failed
}
