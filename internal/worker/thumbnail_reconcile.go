package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
)

const (
	// thumbnailReconcileRowLimit は 1 パスで拾う録画候補の既定上限。
	thumbnailReconcileRowLimit = 1000
)

// ThumbnailReconcileWorker は missing thumbnail / seek_tiles と、チャプターにより
// 再選択が必要な thumbnail を定期パスで見つける queue worker。
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
// キューを使う。queue adapter のキュー単位並列度はジョブ種を区別しないため、
// thumbnail の抽出中はこのパスも待つが、次の周期に同じ desired−observed を
// 再確認すればよい。
//
// missing thumbnail と再選択候補はどちらも poster の一部として同じキューへ積む。
// **seek_tiles も同じパスが埋める**が、候補集合は別なので poster の窓と seek_tiles
// の窓は独立に巡回する。どちらかだけが上限に張り付いたとき、共通カーソルで他方の
// 候補を飛ばさないためである。
// 3 つの窓（missing poster / 再選択 / seek_tiles）は再開位置を独立に持つが、継続
// ジョブは 1 本にまとめる。窓ごとに分けると、満杯の窓の数だけ継続チェーンが並走し、
// どの窓を評価するかを引数で選ぶ分岐も要る。1 本なら毎ページ全窓を評価するだけで済む。
//
// 引数の Done フラグは、チェーンの中で一度末尾まで読んだ窓を以後のページで照会しない
// ための印である。無いと、短い窓は次のページで再開位置 0 に戻って先頭から読み直し、
// 先頭の候補を投入し直す（他の窓が満杯で継続が続く間、毎ページ繰り返す）。
type ThumbnailReconcileWorker struct {
	Enqueuer jobq.Enqueuer
	// Media はサムネイル・シークタイルの列挙の読み取り契約。nil は reconcile を呼ばない部分テストでのみ許容する。
	Media    store.MediaReconcile
	RowLimit int32
}

// Work は thumbnail reconcile を全件または指定録画について実行する。
//
// missing thumbnail、thumbnail の再選択、seek_tiles は recording_id 単位で独立に
// keyset pagination する。各窓が RowLimit 件以上を返したときは最後に見た ID を
// 継続ジョブ引数へ載せる。次の定期パスは先頭から始めるため、恒久的に失敗する録画が
// 先頭に残っても、継続ジョブが後続候補へ進む。
func (w *ThumbnailReconcileWorker) Work(ctx context.Context, args jobs.ThumbnailReconcileArgs) error {
	recordingID := args.RecordingID
	if w.Enqueuer == nil {
		return fmt.Errorf("thumbnail reconcile: job enqueuer is nil")
	}

	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = thumbnailReconcileRowLimit
	}

	after := args.AfterRecordingID
	reselectAfter := args.AfterReselectRecordingID
	var targetID *int64
	if recordingID != 0 {
		targetID = &recordingID
	}
	var rows []int64
	if !args.MissingDone {
		var err error
		rows, err = w.Media.ListMissingThumbnailRecordings(ctx, store.MediaReconcilePage{
			AfterRecordingID: after,
			RecordingID:      targetID,
			RowLimit:         rowLimit,
		})
		if err != nil {
			// DB 取得に失敗したパスは完走していないので、再開位置を変えない。
			return fmt.Errorf("listing missing thumbnail recordings: %w", err)
		}
	}

	var reselectRows []store.ThumbnailReselectCandidate
	if !args.ReselectDone {
		var err error
		reselectRows, err = w.Media.ListThumbnailReselectCandidates(ctx, store.MediaReconcilePage{
			AfterRecordingID: reselectAfter,
			RecordingID:      targetID,
			RowLimit:         rowLimit,
		})
		if err != nil {
			return fmt.Errorf("listing thumbnail reselection candidates: %w", err)
		}
	}

	failed := 0
	for _, recordingID := range rows {
		if err := enqueueJob(ctx, w.Enqueuer, jobs.ThumbnailJobArgs{RecordingID: recordingID}); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue missing thumbnail",
				"recording_id", recordingID, "err", err)
		}
	}
	for _, row := range reselectRows {
		timeline, hasTimeline := thumbnailTimeline(row)
		if !hasTimeline {
			continue
		}
		inputs := thumbnailInputsOf(row)
		if !thumbnailNeedsReselect(row.SeekMs, inputs, timeline) {
			continue
		}
		if err := enqueueJob(ctx, w.Enqueuer, jobs.ThumbnailJobArgs{RecordingID: row.RecordingID}); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue reselection",
				"recording_id", row.RecordingID, "err", err)
		}
	}

	seekTilesFailed, seekTilesAfter, seekTilesWindowFull, seekTilesDone := w.enqueueMissingSeekTiles(
		ctx, w.Enqueuer, rowLimit, args.AfterSeekTilesRecordingID, args.SeekTilesDone, targetID,
	)
	failed += seekTilesFailed

	if recordingID == 0 {
		metrics.ThumbnailReconcileCandidates.Set(float64(len(rows) + len(reselectRows)))
		metrics.ThumbnailReconcileLastPass.SetToCurrentTime()

		var nextAfter int64
		missingWindowFull := !args.MissingDone && int32(len(rows)) >= rowLimit
		missingDone := args.MissingDone || !missingWindowFull
		if missingWindowFull {
			nextAfter = rows[len(rows)-1]
		}

		var nextReselectAfter int64
		reselectWindowFull := !args.ReselectDone && int32(len(reselectRows)) >= rowLimit
		reselectDone := args.ReselectDone || !reselectWindowFull
		if reselectWindowFull {
			nextReselectAfter = reselectRows[len(reselectRows)-1].RecordingID
		}

		continuation := jobs.ThumbnailReconcileArgs{
			AfterRecordingID:          nextAfter,
			AfterReselectRecordingID:  nextReselectAfter,
			AfterSeekTilesRecordingID: seekTilesAfter,
			MissingDone:               missingDone,
			ReselectDone:              reselectDone,
			SeekTilesDone:             seekTilesDone,
		}
		if missingWindowFull || reselectWindowFull || seekTilesWindowFull {
			jobs.EnqueueContinuation(ctx, w.Enqueuer, continuation,
				"candidate windows are full; continuation enqueued",
				[]any{"after_recording_id", nextAfter,
					"after_reselect_recording_id", nextReselectAfter,
					"after_seek_tiles_recording_id", seekTilesAfter},
				[]any{"row_limit", rowLimit,
					"missing_window_full", missingWindowFull,
					"reselection_window_full", reselectWindowFull,
					"seek_tiles_window_full", seekTilesWindowFull})
		}

		if len(rows) > 0 || len(reselectRows) > 0 || failed > 0 {
			slog.Info("thumbnail_reconcile: pass complete",
				"missing_candidates", len(rows), "reselection_candidates", len(reselectRows),
				"failed", failed, "row_limit", rowLimit,
				"after_recording_id", nextAfter,
				"after_reselect_recording_id", nextReselectAfter,
				"after_seek_tiles_recording_id", seekTilesAfter)
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
// 持つ。戻り値は投入失敗数、次のカーソル、満杯かどうか、チェーン内で完了したか。
func (w *ThumbnailReconcileWorker) enqueueMissingSeekTiles(
	ctx context.Context,
	enqueuer jobq.Enqueuer,
	rowLimit int32,
	after int64,
	done bool,
	targetID *int64,
) (failed int, nextAfter int64, windowFull bool, nextDone bool) {
	if done {
		return 0, 0, false, true
	}
	rows, err := w.Media.ListMissingSeekTilesRecordings(ctx, store.MediaReconcilePage{
		AfterRecordingID: after,
		RecordingID:      targetID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		// DB 取得に失敗したパスは完走していないので、再開位置を変えない。
		slog.Error("thumbnail_reconcile: listing missing seek tiles failed", "err", err)
		return 1, after, false, false
	}

	for _, recordingID := range rows {
		if err := enqueueJob(ctx, enqueuer, jobs.SeekTilesJobArgs{RecordingID: recordingID}); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue missing seek tiles",
				"recording_id", recordingID, "err", err)
		}
	}

	if targetID == nil {
		if int32(len(rows)) >= rowLimit {
			nextAfter = rows[len(rows)-1]
			windowFull = true
		} else {
			nextDone = true
		}
		if len(rows) > 0 || failed > 0 {
			slog.Info("thumbnail_reconcile: seek tiles pass complete",
				"candidates", len(rows), "failed", failed, "row_limit", rowLimit, "after_recording_id", nextAfter)
		}
	} else if len(rows) > 0 || failed > 0 {
		slog.Info("thumbnail_reconcile: targeted seek tiles pass complete",
			"recording_id", *targetID, "candidates", len(rows), "failed", failed)
	}
	return failed, nextAfter, windowFull, nextDone
}
