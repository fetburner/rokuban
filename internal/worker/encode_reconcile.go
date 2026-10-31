package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
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
//   - C2（コスト）: 1 パスが examine する候補録画は L 件まで。不足と作り直しは
//     同じ一覧で録画単位の窓に入る。
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
	// Media は encode reconcile の列挙の読み取り契約。nil は reconcile を呼ばない部分テストでのみ許容する。
	Media store.MediaReconcile

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

	// Transfers は置き場ごとの転送待ちの件数を引く契約。nil なら背圧をかけない。
	Transfers store.StorageTransfer
	// StoragePolicy は storage.transfer.backlog_limit と、転送待ちを数える規則を持つ。
	// backlog_limit の無い置き場には背圧をかけない。
	StoragePolicy config.StorageConfig
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
// 候補の抽出を ListMissingEncodeProfiles でまとめて行い、不足と未充足の作り直しを
// 同じ録画単位の窓で扱う。これにより、候補ごとの原本・ポリシー・encoded の再取得を
// 避ける。known_profiles も SQL に渡して、設定から消えたプロファイルや空の
// プロファイル名を投入対象から外す。対象録画を指定したパスも同じクエリで判定し、
// 全件パスの窓やメトリクスに触れない。
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
	// 通常候補の cut: true は所有（= ユーザーが確認済み）の行がある録画に限る。
	// 作り直しは要求時の keep が確認を表すので、SQL 側で別条件として絞る。
	cut := w.Profiles.CutProfileNames()

	// 対象録画のパスは継続カーソルを使わず、全件パスから独立させる。
	after := args.AfterRecordingID
	var targetID *int64
	if recordingID != 0 {
		targetID = &recordingID
	}
	missing, err := w.Media.ListMissingEncodeProfiles(ctx, store.MissingEncodeProfilesParams{
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
	for _, row := range missing {
		if len(candidates) == 0 || candidates[len(candidates)-1] != row.RecordingID {
			candidates = append(candidates, row.RecordingID)
		}
	}
	// 投入先の置き場は、原本のコピーがある置き場。候補の件数に比例して問い合わせないよう、まとめて引く。
	originalStores, err := originalStoresOf(ctx, w.Media, candidates)
	if err != nil {
		return fmt.Errorf("encode reconcile: %w", err)
	}
	// 件数の集計は件数に比例して重いので、判断と表示に要るパスだけで数える（理由は backpressuredStores）。
	limited := w.backlogLimitedStores()
	blocked, err := w.backpressuredStores(ctx, limited, needsBacklogCount(recordingID == 0 && after == 0, missing, originalStores, limited))
	if err != nil {
		return fmt.Errorf("encode reconcile: checking transfer backpressure: %w", err)
	}
	candidates = candidates[:0]
	rebuildQueued, failed := 0, 0
	cutSet := w.Profiles.CutProfileSet()
	for _, row := range missing {
		if len(candidates) == 0 || candidates[len(candidates)-1] != row.RecordingID {
			candidates = append(candidates, row.RecordingID)
		}
		if row.IsRebuild && row.DecodeErr != nil {
			failed++
			slog.Error("encode_reconcile: decoding requested keep ranges",
				"recording_id", row.RecordingID, "profile", row.Profile, "err", row.DecodeErr)
			continue
		}
		// 転送待ちが上限に達した置き場には encode を積まない（背圧）。原本の ingest は止めない。
		if blocked[originalStores[row.RecordingID]] {
			continue
		}
		// 全区間カットの通常候補は投入しても observeEncodePlan が必ず失敗する。
		// SQL は keep の導出（Derive）を持てないので候補は広めに出し、ここで落とす。
		// 作り直しの keep は API が空を拒み、チャプター未所有でも有効なので照合しない。
		// 窓の回転は candidates（落とす前）で数えるので進み続ける。
		if !row.IsRebuild {
			if _, isCut := cutSet[row.Profile]; isCut {
				keep, _, err := w.Media.GetCurrentCutKeep(ctx, row.RecordingID)
				if err != nil {
					failed++
					slog.Error("encode_reconcile: loading cut keep", "recording_id", row.RecordingID, "err", err)
					continue
				}
				if len(keep) == 0 {
					continue
				}
			}
		}
		args, err := newEncodeJobArgs(ctx, w.Media, row.RecordingID, originalStores[row.RecordingID], row.Profile, w.Profiles)
		if err != nil {
			failed++
			slog.Error("encode_reconcile: building encode job args",
				"recording_id", row.RecordingID, "profile", row.Profile, "err", err)
			continue
		}
		if row.IsRebuild {
			args.RequestedCutKeep, err = json.Marshal(row.RequestedKeep)
			if err != nil {
				failed++
				slog.Error("encode_reconcile: encoding requested keep ranges",
					"recording_id", row.RecordingID, "profile", row.Profile, "err", err)
				continue
			}
		}
		if err := enqueueJob(ctx, w.Enqueuer, args); err != nil {
			failed++
			slog.Error("encode_reconcile: failed to enqueue encode candidate",
				"recording_id", row.RecordingID, "profile", row.Profile, "err", err)
			continue
		}
		if row.IsRebuild {
			rebuildQueued++
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
			jobs.EnqueueContinuation(ctx, w.Enqueuer, continuation,
				"candidate window is full; continuation enqueued",
				[]any{"after_recording_id", continuationAfter},
				[]any{"row_limit", rowLimit})
		}

		w.reportUnsatisfiable(ctx, known)
		w.reportAwaitingReview(ctx)

		if len(candidates) > 0 || failed > 0 {
			slog.Info("encode_reconcile: pass complete",
				"candidates", len(candidates), "rebuild_requests", rebuildQueued,
				"failed", failed, "row_limit", rowLimit,
				"after_recording_id", after, "continuation_after_recording_id", continuationAfter)
		}
	} else if len(candidates) > 0 || failed > 0 {
		slog.Info("encode_reconcile: targeted pass complete",
			"recording_id", recordingID, "candidates", len(candidates),
			"rebuild_requests", rebuildQueued, "failed", failed)
	}
	return nil
}

// needsBacklogCount は、このパスで転送待ちの件数を数えるかを返す。
// 全件パスの先頭（firstFullPage）か、投入候補のどれかの原本が上限付きの置き場にあるときに数える。
func needsBacklogCount(firstFullPage bool, missing []store.MissingEncodeProfile, originalStores map[int64]string, limited []string) bool {
	return firstFullPage || slices.ContainsFunc(missing, func(row store.MissingEncodeProfile) bool {
		return slices.Contains(limited, originalStores[row.RecordingID])
	})
}

// backlogLimitedStores は storage.transfer.backlog_limit を設定した置き場を名前順に返す。
func (w *EncodeReconcileWorker) backlogLimitedStores() []string {
	var limited []string
	for name := range w.StoragePolicy.Transfer.BacklogLimit {
		if w.StoragePolicy.TransferBacklogLimit(name) > 0 {
			limited = append(limited, name)
		}
	}
	slices.Sort(limited)
	return limited
}

// backpressuredStores は、転送待ちが storage.transfer.backlog_limit に達していて encode を投入しない置き場を返す。
//
// 件数を数えるのは refresh のときと、止まっている置き場があるときだけ。呼び出し側は次のどれかで refresh を立てる:
// 全件パスの先頭（ゲージの更新。全件パスは一定周期で走る）、候補の原本が上限付きの置き場にある
// （止めるかどうかを今の件数で決める必要がある）。止まっている置き場があるかは、直近の件数を DB から読んで
// 今の上限と比べて決める（解けたことを次のパスで見せる）。どちらでもないパスは数えず nil を返す。
// 集計 1 回は資産数に比例して重く（計測: media_assets 2 万行で中央値約 2.1 秒、6 万行で約 6.6 秒）、
// ingest ごとの targeted pass と継続ページの全てで数えると、その分だけ DB を占有するためである。
//
// 状態は DB の transfer_backlog_snapshots に置く。cleanup キューは ScaledJob の --once で 1 件ごとに
// プロセスが終わるので、メモリに持つと毎回空から始まり、Warn が毎パス出て、止まっている間の数え直しも起きない。
// 数えたときは、置き場ごとの件数を保存し、書き換える前の件数と今の件数から止まり始め・解けたときだけ
// Warn / Info を出す。前の件数の読みと保存は 1 つのトランザクションで直列にしてあるので、並行する 2 パスでも
// 変化を見るのは 1 つだけである。数え始めた時刻も保存し、保存済みより古い件数は捨てる。止める判断は
// 書き込み後に DB に残った件数で行うので、捨てたときもゲージと同じ件数になる。捨てた置き場は Warn / Info を出さない。ゲージ（rokuban_storage_transfer_backlog_assets / rokuban_encode_backpressure）は、
// 保存した件数を scrape のたびに metrics.TransferBacklogCollector が読んで出す。
//
// Transfers が nil のときは何もしない。上限を設定した置き場が無いときは、DB に触れない
// （全件パスの先頭だけ、残っている行を消す）。
func (w *EncodeReconcileWorker) backpressuredStores(ctx context.Context, limited []string, refresh bool) (map[string]bool, error) {
	if w.Transfers == nil {
		return nil, nil
	}
	if len(limited) == 0 {
		// 上限の無い構成の targeted pass は DB に触らない。残った行の掃除は全件パスの先頭だけで行う。
		if refresh {
			if _, err := w.Transfers.RecordTransferBacklog(ctx, nil, time.Time{}); err != nil {
				return nil, fmt.Errorf("clearing transfer backlog snapshots: %w", err)
			}
		}
		return nil, nil
	}
	if !refresh {
		saved, err := w.Transfers.ReadTransferBacklog(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading transfer backlog snapshots: %w", err)
		}
		if !slices.ContainsFunc(limited, func(name string) bool {
			snap, ok := saved[name]
			return ok && snap.Assets >= int64(w.StoragePolicy.TransferBacklogLimit(name))
		}) {
			return nil, nil
		}
	}
	rules, err := w.StoragePolicy.OriginalRulesJSON()
	if err != nil {
		return nil, fmt.Errorf("building original rules: %w", err)
	}
	// 数え始めた時刻を保存する。並行する別のパスの、より新しい件数を古い件数で戻さないため。
	// 出所は DB の時計に 1 つにする（Pod ごとの時計のずれが比較に入らないように）。clock_timestamp() を選ぶのは、
	// now() がトランザクション開始時刻なのに対し、これが読んだ時点の時刻で「数え始める直前」に合うため。
	countedAt, err := w.Transfers.DatabaseClock(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading database clock: %w", err)
	}
	counts, err := w.Transfers.CountMediaAssetTransferBacklogBySourceStore(ctx, rules, w.StoragePolicy.FSStoreNames(), limited, w.StoragePolicy.Derived, storageTransferAttemptLimit)
	if err != nil {
		return nil, fmt.Errorf("counting transfer backlog by source store: %w", err)
	}
	// 件数の無い置き場は 0 件として保存する（行が無いと「数えていない」と区別できない）。
	current := make(map[string]int64, len(limited))
	for _, name := range limited {
		current[name] = counts[name]
	}
	written, err := w.Transfers.RecordTransferBacklog(ctx, current, countedAt)
	if err != nil {
		return nil, fmt.Errorf("recording transfer backlog: %w", err)
	}
	blocked := make(map[string]bool)
	for _, name := range limited {
		limit := w.StoragePolicy.TransferBacklogLimit(name)
		write := written[name]
		// 止める判断は、DB に残った件数で行う。保存済みのほうが新しいときは、ゲージと同じ新しい件数になる。
		backlog := write.Current
		stopped := backlog >= int64(limit)
		if stopped {
			blocked[name] = true
		}
		if !write.Applied {
			// 状態の変化は、新しい件数を書いたパスが見る。
			continue
		}
		wasStopped := write.HadPrevious && write.Previous >= int64(limit)
		switch {
		case stopped && !wasStopped:
			slog.Warn("encode_reconcile: transfer backlog reached the limit; not enqueuing encodes into this store",
				"store", name, "backlog", backlog, "limit", limit)
		case !stopped && wasStopped:
			slog.Info("encode_reconcile: transfer backlog is below the limit; enqueuing encodes into this store again",
				"store", name, "backlog", backlog, "limit", limit)
		}
	}
	return blocked, nil
}

// reportUnsatisfiable は「凍結済みの desired が現在の encode.profiles に無い」
// ために投入対象から外れている録画を数え、ゲージとログに出す。
//
// パスの本体（投入）とは独立した観測なので、失敗してもパスは成功のまま終える
// （ここで error を返すと、投入は済んでいるのにジョブが再試行される）。
func (w *EncodeReconcileWorker) reportUnsatisfiable(ctx context.Context, known []string) {
	rows, err := w.Media.ListUnsatisfiableEncodeProfiles(ctx, known)
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
func (w *EncodeReconcileWorker) reportAwaitingReview(ctx context.Context) {
	cut := w.Profiles.CutProfileNames()
	if len(cut) == 0 {
		metrics.CutAwaitingReview.Reset()
		return
	}
	rows, err := w.Media.ListCutAwaitingReview(ctx, cut)
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
