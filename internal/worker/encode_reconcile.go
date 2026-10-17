package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
)

const (
	// encodeReconcileRowLimit は 1 パスで拾う候補録画の既定上限
	// （EncodeReconcileWorker.RowLimit で上書きできる）。候補がこの数に達したときは
	// recording_id を引き継ぐ継続ジョブを投入し、次のプロセスが残りを調べる。
	// そのため、この定数は 1 パスで調べる候補数を制限するコストのつまみである。
	encodeReconcileRowLimit = 1000
)

// EncodeReconcileWorker は desired（recording_encode_policy.encode_profiles）−
// observed（active な encoded media_assets）の差分を定期的に埋める queue worker
// （issue #163）。
//
// エンコード投入は本来レベルトリガー（不変条件 5）だが、実際に差分を埋める
// きっかけは長らくヒント 2 経路（ingest 完了時のベストエフォート投入と
// POST /api/recordings/{id}/encode-profiles）しか無かった。ヒント投入の失敗と
// エッジ record の削除成功が両方起きると、record_sweep も ingest ジョブを
// 再投入しない（エッジに record が無い）ため、コミット済みの録画が誰にも
// 再投入されず黙ってエンコードされないままになる。このワーカーがその
// 「真実を定期的に再取得する」側である。
//
// # 試行上限に達した encode の扱い
//
// 「encoded が無い」という観測が続く間は、15 分ごとのパスで encode を投入する。
// ただし recording_encode_attempts が failed かつ domain の attempt_limit に達した
// (recording_id, profile) は候補から外す。これをしないと、すでに開始時に破棄される
// 引数を `--once` の Pod が繰り返し拾い、候補窓の先頭を塞ぐ。失敗回数が上限未満の
// failed 行と running 行は候補に残す。利用者が再要求すると failed 行が削除され、
// 次のパスで候補に戻る。
//
// 真実はジョブ履歴ではなく media_assets の有無なので、上限に達する前の失敗は
// レベルトリガーとして再投入を続ける。**入力ファイルの破損など恒久失敗は、上限に
// 達するまでは再試行され、上限後は利用者の再要求まで停止する。**
// 設定から消えたプロファイルは投入しても EncodeWorker が `unknown encode profile` で
// 弾くだけ（encode.go）なので、下記の絞り込みで候補から外し、別のメトリクスとログで
// 見えるようにする。
//
// # 候補窓の継続
//
// 候補は recording_id 昇順 + LIMIT（RowLimit）で切る。このパス自身は候補を
// 減らさない（減らすのは encode ジョブ側）ので、先頭の候補が恒久的に失敗しても
// 後続の録画を調べられるよう、上限まで候補を見たら最後の recording_id を
// EncodeReconcileArgs.AfterRecordingID に載せた継続ジョブを投入する。カーソルは
// ジョブ引数にあるため、`--once` が次のパスでプロセスを作り直しても続きから進む。
// 上限未満のページでは継続を投入せず、次の定期パスが先頭から始める。
//
// 判定基準（RowLimit の値 L に依存しない形で書く。L = 1 パスの行上限、
// S = 定期パスが開始した時点の候補集合）:
//
//   - C1（被覆）: 継続ジョブの連鎖が末尾まで進めば、S の全要素を
//     examine する。連鎖の途中で S に入った録画のうちカーソルより前にあるものは
//     次の定期パスで拾う。TestEncodeReconcileWorker_ContinuationSurvivesFreshWorker が固定する。
//   - C2（コスト）: 1 パスが examine する候補録画は L 件まで。作り直し要求の
//     全件列挙はこの上限の外にある。
//     TestEncodeReconcileWorker_RowLimitCapsWorkPerPass が固定する。
//   - C3（通常運転で不活性）: 候補数が L 件未満なら継続ジョブを投入しない。
//
// 定期パスの引数 `{}` は、River の一意性が running を含む状態で同じ引数の重複を
// 合流させる（`internal/jobq/riveradapter/insertopts_test.go` の
// TestMakeInsertOptsQueueAndUnique）。rescuer は worker の Timeout
// （encode_reconcile は 5 分）が過ぎるまで running の行を回収しないので、実行中の
// 定期パスが回収されて二重に走ることはない。
// 継続ジョブはカーソルが異なる別の引数なので定期パスと並走しうるが、encode の
// 投入は (recording_id, profile) の同じ引数で合流する。
//
// 「設定から消えたプロファイル」は恒久候補を過去録画に一斉に作る原因になるため、
// desired を known_profiles（現在の encode.profiles）で絞って候補から外している。
// 投入しても EncodeWorker が `unknown encode profile` で弾くだけ（encode.go）で、
// 何も前進しないためである。落とした数は metrics.EncodeReconcileUnsatisfiable と
// ログに出す（黙って落とすと、「エンコードされない録画が静かに増える」症状を
// 別の原因で再現してしまう）。
//
// site 照合ガード（issue #139）は不要: EncodeReconcileArgs は site を持たず、
// mirakc にもファイルにも触れない（DB 読み + enqueue のみ）。どの site に
// 束縛された worker が拾っても結果は同じ。
type EncodeReconcileWorker struct {
	Enqueuer jobq.Enqueuer
	Pool     *pgxpool.Pool

	// Profiles は現在の encode.profiles（config.EncodeConfig）。desired の
	// 絞り込みに名前だけを使う（ffmpeg は起動しない）。
	//
	// 空（プロファイル未設定）でも黙らない: 候補は 0 件になるが、凍結済みの
	// desired は全部 metrics.EncodeReconcileUnsatisfiable と Warn に出る
	// （TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent）。
	// これは ProfileNames() が空設定でも non-nil を返すことに依存している ---
	// nil を渡すと SQL 側が `= ANY(NULL)` で NULL になり、候補も検出も同時に
	// 落ちる（encode_reconcile.sql のコメント参照）。
	Profiles config.EncodeConfig

	// RowLimit は 1 パスで拾う候補の上限。0 なら encodeReconcileRowLimit。
	// 上限に張り付いたときの挙動をテストから再現するために可変にしてある。
	RowLimit int32
}

// enqueueEncodeReconcile は指定録画の reconcile を投入する。
// ingest 後の best-effort 投入なので、失敗しても呼び出し元の処理は巻き戻さない。
func enqueueEncodeReconcile(ctx context.Context, enqueuer jobq.Enqueuer, recordingID int64) {
	if err := enqueueJob(ctx, enqueuer, jobs.EncodeReconcileArgs{RecordingID: recordingID}); err != nil {
		slog.Error("encode_reconcile: failed to enqueue targeted pass", "recording_id", recordingID, "err", err)
	}
}

// Work は encode reconcile を全件または指定録画について実行する。
//
// 候補の抽出と不足プロファイルの判定を ListMissingEncodeProfiles でまとめて行う。
// これにより、候補ごとの原本・ポリシー・encoded の再取得を避ける。known_profiles
// も SQL に渡して、設定から消えたプロファイルや空のプロファイル名を投入対象から
// 外す。対象録画を指定したパスも同じクエリで判定し、全件パスの窓やメトリクスに
// 触れない。
//
// 1 件の失敗でパス全体を止めない（record_sweep の processRecord・
// delete_reconcile の deleteMediaAsset と同じ判断）。次パスが同じ候補を
// 拾い直す。
func (w *EncodeReconcileWorker) Work(ctx context.Context, args jobs.EncodeReconcileArgs) error {
	recordingID := args.RecordingID
	if recordingID != 0 && args.AfterRecordingID != 0 {
		return fmt.Errorf("encode reconcile: recording_id and after_recording_id cannot both be set: %w", jobq.ErrDiscard)
	}
	if w.Enqueuer == nil {
		// このジョブの主目的が「encode ジョブを実際に投入すること」なので、client が取れないことを
		// 黙った no-op にすると取りこぼしの回復そのものが消える。
		return fmt.Errorf("encode reconcile: job enqueuer is nil")
	}

	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = encodeReconcileRowLimit
	}
	known := w.Profiles.ProfileNames()
	// cut: true のプロファイルは所有（= ユーザーが確認済み）の行がある録画にしか
	// 投入しない（SQL 側の同じ述語。encode_reconcile.sql のコメント参照）。
	cut := w.Profiles.CutProfileNames()

	// 対象録画のパスは継続カーソルを使わず、全件パスから独立させる。
	after := args.AfterRecordingID
	var targetID *int64
	if recordingID != 0 {
		targetID = &recordingID
	}
	q := sqlcgen.New(w.Pool)
	rebuildQueued, failed, err := w.reconcileRebuildRequests(ctx, q, targetID)
	if err != nil {
		return fmt.Errorf("reconciling encoded asset rebuild requests: %w", err)
	}
	missing, err := q.ListMissingEncodeProfiles(ctx, sqlcgen.ListMissingEncodeProfilesParams{
		AfterRecordingID: after,
		RecordingID:      targetID,
		KnownProfiles:    known,
		CutProfiles:      cut,
		AttemptLimit:     jobs.EncodeAttemptLimit,
		RowLimit:         rowLimit,
	})
	if err != nil {
		// 再開位置は触らない。次パスが同じ位置から引き直す。
		return fmt.Errorf("listing missing encode profiles: %w", err)
	}

	// クエリは recording_id ごとに全不足プロファイルを返す。結果は recording_id
	// 昇順なので、ここで候補録画を一度だけ数える。RowLimit は profile 行数ではなく
	// この録画数に適用され、window の再開位置も録画単位で進む。
	candidates := make([]int64, 0, len(missing))
	cutSet := w.Profiles.CutProfileSet()
	for _, row := range missing {
		if len(candidates) == 0 || candidates[len(candidates)-1] != row.RecordingID {
			candidates = append(candidates, row.RecordingID)
		}
		// 全区間カットの cut は投入しても loadCutContext が必ず失敗する。SQL は
		// keep の導出（Derive）を持てないので候補は広めに出し、ここで落とす。
		// 窓の回転は candidates（落とす前）で数えるので進み続ける。
		if _, isCut := cutSet[row.Profile]; isCut {
			keep, _, err := currentCutKeep(ctx, q, row.RecordingID)
			if err != nil {
				failed++
				slog.Error("encode_reconcile: loading cut keep", "recording_id", row.RecordingID, "err", err)
				continue
			}
			if len(keep) == 0 {
				continue
			}
		}
		args, err := newEncodeJobArgs(ctx, q, row.RecordingID, row.Profile, w.Profiles)
		if err != nil {
			failed++
			slog.Error("encode_reconcile: building encode job args",
				"recording_id", row.RecordingID, "profile", row.Profile, "err", err)
			continue
		}
		if err := enqueueJob(ctx, w.Enqueuer, args); err != nil {
			failed++
			slog.Error("encode_reconcile: failed to enqueue missing encodes",
				"recording_id", row.RecordingID, "profile", row.Profile, "err", err)
		}
	}

	if recordingID == 0 {
		metrics.EncodeReconcileCandidates.Set(float64(len(candidates)))
		metrics.EncodeReconcileLastPass.SetToCurrentTime()

		// 上限まで候補を見たページは、最後の recording_id を引数に継続ジョブを積む。
		// 続きが無い場合も追加 query をせず次ページを 1 回だけ走らせ、0 件なら連鎖を止める。
		windowFull := int32(len(candidates)) >= rowLimit
		var continuationAfter int64
		if windowFull {
			continuationAfter = candidates[len(candidates)-1]
			continuation := jobs.EncodeReconcileArgs{AfterRecordingID: continuationAfter}
			if err := enqueueJob(ctx, w.Enqueuer, continuation); err != nil {
				slog.Error("encode_reconcile: failed to enqueue continuation",
					"after_recording_id", continuationAfter, "err", err)
			} else {
				slog.Warn("encode_reconcile: candidate window is full; continuation enqueued",
					"row_limit", rowLimit, "after_recording_id", continuationAfter)
			}
		}

		w.reportUnsatisfiable(ctx, q, known)
		w.reportAwaitingReview(ctx, q)

		if len(candidates) > 0 || rebuildQueued > 0 || failed > 0 {
			slog.Info("encode_reconcile: pass complete",
				"candidates", len(candidates), "rebuild_requests", rebuildQueued,
				"failed", failed, "row_limit", rowLimit,
				"after_recording_id", after, "continuation_after_recording_id", continuationAfter)
		}
	} else if len(candidates) > 0 || rebuildQueued > 0 || failed > 0 {
		slog.Info("encode_reconcile: targeted pass complete",
			"recording_id", recordingID, "candidates", len(candidates),
			"rebuild_requests", rebuildQueued, "failed", failed)
	}
	return nil
}

// reconcileRebuildRequests は満たされていない作り直し（ListEncodedAssetRebuildRequests）
// に、押した時点の keep を束縛した encode ジョブを投入する。
//
// 要求行はここでは消さない（消すのは api だけ）。チャプターが後から変わっても、押した
// 時点の keep で描く。設定から外れた profile と cut でない profile は投入せず、
// 設定が戻るまで行を保持する。cut でない設定は C-1 の起動時検査
// （CheckEncodeProfileMeaning）が拒むので、ここに来るのは、検査より後に設定を変えた
// 場合だけである。
func (w *EncodeReconcileWorker) reconcileRebuildRequests(
	ctx context.Context,
	q *sqlcgen.Queries,
	targetID *int64,
) (queued, failed int, err error) {
	requests, err := q.ListEncodedAssetRebuildRequests(ctx, targetID)
	if err != nil {
		return 0, 0, fmt.Errorf("listing encoded asset rebuild requests: %w", err)
	}
	for _, request := range requests {
		requestedKeep, err := db.KeepRangesFromMultirange(request.RequestedKeep)
		if err != nil {
			failed++
			slog.Error("encode_reconcile: decoding requested keep ranges",
				"recording_id", request.RecordingID, "profile", request.Profile, "err", err)
			continue
		}
		if profile, configured := w.Profiles.Profile(request.Profile); !configured || !profile.Cut {
			continue
		}
		encodeArgs, err := newEncodeJobArgs(ctx, q, request.RecordingID, request.Profile, w.Profiles)
		if err != nil {
			failed++
			slog.Error("encode_reconcile: building encode args for rebuild request",
				"recording_id", request.RecordingID, "profile", request.Profile, "err", err)
			continue
		}
		encodeArgs.RequestedCutKeep, err = json.Marshal(requestedKeep)
		if err != nil {
			failed++
			slog.Error("encode_reconcile: encoding requested keep ranges",
				"recording_id", request.RecordingID, "profile", request.Profile, "err", err)
			continue
		}
		if err := enqueueJob(ctx, w.Enqueuer, encodeArgs); err != nil {
			failed++
			slog.Error("encode_reconcile: failed to enqueue rebuild request",
				"recording_id", request.RecordingID, "profile", request.Profile, "err", err)
			continue
		}
		queued++
	}
	return queued, failed, nil
}

// reportUnsatisfiable は「凍結済みの desired が現在の encode.profiles に無い」
// ために投入対象から外れている録画を数え、ゲージとログに出す。
//
// パスの本体（投入）とは独立した観測なので、失敗してもパスは成功のまま終える
// （ここで error を返すと、投入は済んでいるのにジョブが再試行される）。
func (w *EncodeReconcileWorker) reportUnsatisfiable(ctx context.Context, q *sqlcgen.Queries, known []string) {
	rows, err := q.ListUnsatisfiableEncodeProfiles(ctx, known)
	if err != nil {
		slog.Error("encode_reconcile: listing unsatisfiable encode profiles", "err", err)
		return
	}
	// 直前のパスで報告したプロファイルが解消した（設定に戻した）場合に
	// ゲージが張り付かないよう、毎回作り直す。
	metrics.EncodeReconcileUnsatisfiable.Reset()
	for _, r := range rows {
		metrics.EncodeReconcileUnsatisfiable.WithLabelValues(r.Profile).Set(float64(r.Recordings))
		slog.Warn("encode_reconcile: frozen encode profile is not in the current configuration; these recordings will never be encoded",
			"profile", r.Profile, "recordings", r.Recordings)
	}
}

// reportAwaitingReview は「cut プロファイルを凍結しているのに、まだチャプターを
// 確認していない」録画数をゲージに出す。
//
// **これは失敗ではないので Warn にしない**（ユーザーが確認すれば次のパスが拾う
// 正常な待ち状態）。ログは出すが、数が減らないこと自体は異常ではない。
// EncodeReconcileUnsatisfiable と同じく、解消したプロファイルの系列が
// 張り付かないよう毎回作り直す。
func (w *EncodeReconcileWorker) reportAwaitingReview(ctx context.Context, q *sqlcgen.Queries) {
	cut := w.Profiles.CutProfileNames()
	if len(cut) == 0 {
		metrics.CutAwaitingReview.Reset()
		return
	}
	rows, err := q.ListCutAwaitingReview(ctx, cut)
	if err != nil {
		slog.Error("encode_reconcile: listing cut profiles awaiting review", "err", err)
		return
	}
	metrics.CutAwaitingReview.Reset()
	for _, r := range rows {
		metrics.CutAwaitingReview.WithLabelValues(r.Profile).Set(float64(r.Recordings))
		slog.Info("encode_reconcile: cut profile is waiting for the user to review the detected chapters",
			"profile", r.Profile, "recordings", r.Recordings)
	}
}
