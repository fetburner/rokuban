package thumbnail

import (
	"context"
	"errors"
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
// thumbnail_reconcile は cleanup キュー、ThumbnailWorker / SeekTilesWorker は
// thumbnail キューを使う（TestNewWorkersRegistersThumbnailJobs）。
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
//
// 再選択の SQL 窓は、再選択不要な録画も含めて active thumbnail と CM 検出または所有済み
// chapter のある録画を返す。実際に再選択するかは Go の共通判定が決めるため、SQL が返す
// 行数を N とすると、`ceil(N/thumbnailReconcileRowLimit)` ページを 15 分ごとに処理し、
// N が上限の倍数なら末尾確認の空ページも 1 ページ続く。`--once` では各ページが Pod 1 つに
// なる。この費用は受け入れる（`TestThumbnailReconcile_ReselectionWindowContinuesWithoutEnqueueing`）。
// SQL に同じ判定を複製すると Go の判定とずれうるためである。
type ThumbnailReconcileWorker struct {
	Enqueuer jobq.Enqueuer
	// Media はサムネイル・シークタイルの列挙の読み取り契約。nil は reconcile を呼ばない部分テストでのみ許容する。
	Media    store.MediaReconcile
	RowLimit int32

	// Markers は全件パスの完走時刻を記録する。nil は記録を呼ばない部分テストでのみ許容する。
	Markers store.LoopMarkers
}

// errNoActiveOriginal は、読み取りジョブを積む置き場を決められない（active な原本のコピーが無い）ことを表す。
// 次の reconcile が引き直す。
var errNoActiveOriginal = errors.New("no active original copy to choose a store from")

// enqueueThumbnail は原本のコピーがある置き場（storeName。無ければ空）のキューへ thumbnail を積む。
func (w *ThumbnailReconcileWorker) enqueueThumbnail(ctx context.Context, recordingID int64, storeName string) error {
	if storeName == "" {
		return errNoActiveOriginal
	}
	_, err := w.Enqueuer.Enqueue(ctx, jobs.ThumbnailJobArgs{RecordingID: recordingID, Store: storeName})
	return err
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

	if w.Media == nil {
		return fmt.Errorf("thumbnail reconcile: original locator is nil")
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
	missingStores, err := w.Media.OriginalStores(ctx, rows)
	if err != nil {
		return fmt.Errorf("thumbnail reconcile: locating originals: %w", err)
	}
	for _, recordingID := range rows {
		if err := w.enqueueThumbnail(ctx, recordingID, missingStores[recordingID]); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue missing thumbnail",
				"recording_id", recordingID, "err", err)
		}
	}
	for _, row := range reselectRows {
		timeline := thumbnailTimeline(row)
		inputs := thumbnailInputsOf(row)
		if !thumbnailNeedsReselect(row.SeekMs, inputs, timeline) {
			continue
		}
		if err := w.enqueueThumbnail(ctx, row.RecordingID, row.OriginalStore); err != nil {
			failed++
			slog.Error("thumbnail_reconcile: failed to enqueue reselection",
				"recording_id", row.RecordingID, "err", err)
		}
	}

	seekTilesFailed, seekTilesAfter, seekTilesWindowFull, seekTilesDone, seekTilesListed := w.enqueueMissingSeekTiles(
		ctx, rowLimit, args.AfterSeekTilesRecordingID, args.SeekTilesDone, targetID,
	)
	failed += seekTilesFailed

	if recordingID == 0 {
		metrics.ThumbnailReconcileCandidates.Set(float64(len(rows) + len(reselectRows)))

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
		// シークタイルの一覧を取れなかったパスは、完走として記録しない。取れていれば、
		// 個々の投入が失敗しても記録する（候補ごとの失敗でパスを止めない）。
		if !seekTilesListed {
			return nil
		}
		return store.MarkReconcilePass(ctx, w.Markers, store.ReconcilePassThumbnail)
	}
	if len(rows) > 0 || len(reselectRows) > 0 || failed > 0 {
		slog.Info("thumbnail_reconcile: targeted pass complete",
			"recording_id", recordingID, "missing_candidates", len(rows),
			"reselection_candidates", len(reselectRows), "failed", failed)
	}
	return nil
}

// enqueueMissingSeekTiles は seek_tiles の desired−observed ギャップを埋める。
// thumbnail と同じ窓の形（keyset pagination）だが、全件パスでは再開位置を独立に
// 持つ。戻り値は投入失敗数、次のカーソル、満杯かどうか、チェーン内で完了したか、
// 候補の一覧（と原本の置き場）を取得できたか。一覧を取れなかったときは listed が偽になる。
// 既にチェーン内で完了している窓は、このパスで一覧を取る必要が無いので listed は真になる。
func (w *ThumbnailReconcileWorker) enqueueMissingSeekTiles(
	ctx context.Context,
	rowLimit int32,
	after int64,
	done bool,
	targetID *int64,
) (failed int, nextAfter int64, windowFull bool, nextDone bool, listed bool) {
	if done {
		return 0, 0, false, true, true
	}
	rows, err := w.Media.ListMissingSeekTilesRecordings(ctx, store.MediaReconcilePage{
		AfterRecordingID: after,
		RecordingID:      targetID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		// DB 取得に失敗したパスは完走していないので、再開位置を変えない。
		slog.Error("thumbnail_reconcile: listing missing seek tiles failed", "err", err)
		return 1, after, false, false, false
	}

	tileStores, err := w.Media.OriginalStores(ctx, rows)
	if err != nil {
		slog.Error("thumbnail_reconcile: locating originals for seek tiles failed", "err", fmt.Errorf("locating originals: %w", err))
		return 1, after, false, false, false
	}
	for _, recordingID := range rows {
		var err error
		if storeName := tileStores[recordingID]; storeName == "" {
			err = errNoActiveOriginal
		} else {
			_, err = w.Enqueuer.Enqueue(ctx, jobs.SeekTilesJobArgs{RecordingID: recordingID, Store: storeName})
		}
		if err != nil {
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
	return failed, nextAfter, windowFull, nextDone, true
}
