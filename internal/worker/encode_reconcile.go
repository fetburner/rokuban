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

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
)

const (
	// defaultEncodeReconcileInterval は encode の desired−observed 定期パスの
	// 既定間隔。
	//
	// このパスが**新しく投入する**のは、ヒント経路（ingest 完了時のベストエフォート
	// 投入と POST /api/recordings/{id}/encode-profiles のヒントジョブ）が落ちた
	// ぶんだけである（候補には実行中・待機中のエンコードも含まれるが、それらへの
	// 投入は River の UniqueOpts が合流させるので新しいジョブにはならない）。
	// 落としたヒントの回復が数十分遅れても失うものは無い一方、何時間も放置は
	// したくないので、同じく「普段は新しい仕事を作らないバックストップ」である
	// delete_reconcile と同じ 15 分に揃える。
	defaultEncodeReconcileInterval = 15 * time.Minute

	// encodeReconcileTimeout は 1 パス全体の上限。
	//
	// River の既定（1 分）より長く与える: 候補の読み取りは専用クエリでまとめるが、
	// 不足している (recording_id, profile) ごとの River Insert は残るため、候補数
	// （最大 encodeReconcileRowLimit）とプロファイル数に比例して DB の処理が積み上がる。
	// ffmpeg は一切起動しない（不変条件 4 に触れない。投入するだけ）ので、
	// encode ジョブ本体（Timeout() が -1）のような無制限は要らない。
	encodeReconcileTimeout = 5 * time.Minute

	// encodeReconcileRowLimit は 1 パスで拾う候補録画の既定上限
	// （EncodeReconcileWorker.RowLimit で上書きできる）。
	//
	// **deleteReconcileRowLimit と役割は同じでも性質が違う** --- あちらは拾った
	// 候補をそのパスが消して減らすので窓の先頭が必ず進むが、こちらは候補を消すのが
	// encode ジョブ側なので、パス自身は候補を 1 件も減らさない。以前はこの非対称が
	// 「永久に満たせない候補が先頭に溜まると窓を恒久的に占有する」という到達性の
	// 穴になっていたが、EncodeReconcileWorker.resumeAfter が窓を回すことでコストと
	// 被覆を分離した。**この定数は今は純粋なコストのつまみ**（1 パスあたりの
	// 候補録画数と、それに伴う River Insert 数の上限）で、値を変えても被覆の保証
	// （EncodeReconcileWorker の doc コメント「窓を回す」の C1/C2/C3）は変わらない。
	encodeReconcileRowLimit = 1000
)

// EncodeReconcileWorker は desired（recording_encode_policy.encode_profiles）−
// observed（active な encoded media_assets）の差分を定期的に埋める River ワーカー
// （issue #163）。加えて、encode の Timeout()=-1 では River の JobRescuer が
// 回収しないプロセス死した running ジョブを、同じ encode キューのこのパスで
// job-id advisory lock により回収する（issue #797）。
//
// エンコード投入は本来レベルトリガー（不変条件 5）だが、実際に差分を埋める
// きっかけは長らくヒント 2 経路（ingest 完了時のベストエフォート投入と
// POST /api/recordings/{id}/encode-profiles）しか無かった。ヒント投入の失敗と
// エッジ record の削除成功が両方起きると、record_sweep も ingest ジョブを
// 再投入しない（エッジに record が無い）ため、コミット済みの録画が誰にも
// 再投入されず黙ってエンコードされないままになる。このワーカーがその
// 「真実を定期的に再取得する」側である。
//
// # 挙動の変更: 恒久的に失敗するエンコードは繰り返し投入される
//
// このパスが入るまで、25 回失敗して discarded になった encode ジョブはそこで
// 止まっていた。これからは「encoded が無い」という観測が続く限り 15 分ごとに
// 投入し直す（pendingJobStates に discarded は含まれないので、UniqueOpts は
// discarded 済みの引数を合流させない）。レベルトリガーとしては意図通り ---
// 真実は River のジョブ履歴ではなく media_assets の有無である --- だが、
// **失敗し続けるエンコードは「静かに諦める」から「延々と再試行する」に変わる**。
// 恒久失敗の代表格（設定から消えたプロファイル）は下記の絞り込みで投入対象から
// 外しているので、ここに残るのは入力ファイルの破損など録画単位の失敗である。
//
// # 窓を回す
//
// 候補は recording_id 昇順 + LIMIT（RowLimit）で切る。このパス自身は候補を
// 減らさない（減らすのは encode ジョブ側）ので、「毎パス先頭から」窓を開くと
// 永久に満たせない候補（録画単位の恒久失敗。入力ファイルの破損など）が先頭に
// 溜まったとき、それより後ろの録画に到達できなくなる。これを避けるため、
// 窓は「前パスが止まった位置の続きから」開き、末尾に達したら先頭へ戻る
// （resumeAfter、プロセスローカル。永続化しない）。「設定から消えたプロファイル」
// はこの恒久候補を過去録画に一斉に作る唯一の系統的な原因だが、desired を
// known_profiles（現在の encode.profiles）で絞って候補から外している ---
// 投入しても EncodeWorker が `unknown encode profile` で弾くだけ（encode.go）で、
// 何も前進しないため。落とした数は metrics.EncodeReconcileUnsatisfiable と
// ログに出す（黙って落とすと、このパスが塞いだはずの症状「エンコードされない
// 録画が静かに増える」を別の原因で再現してしまう）。
//
// 判定基準（RowLimit の値 L に依存しない形で書く。L = 1 パスの行上限、
// S = 候補集合）:
//
//   - C1（被覆）: S が増減しない最悪条件下でも、S の任意の要素は連続する
//     ceil(|S|/L)+1 回のパスのどこかで examine される。+1 は「ちょうど L 件で
//     埋まったパス」と「まだ続きがあるパス」を追加の問い合わせなしに区別
//     できないための 1 パス分の余裕（下記コメント参照）。
//     TestEncodeReconcileWorker_WindowRotatesPastStuckCandidates が固定する。
//   - C2（コスト）: 1 パスが examine する候補数は L を超えない。
//     TestEncodeReconcileWorker_RowLimitCapsWorkPerPass が固定する。
//   - C3（通常運転で不活性）: 候補数が L 未満なら resumeAfter は常に 0 のままで、
//     投入対象・件数・順序は今日と一致する。既存の 3 つの回帰テスト
//     （TestEncodeReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord /
//     _DoesNotDoubleEnqueue / TestEncodeReconcileWorker_SkipsProfilesMissingFromConfig）
//     が無変更で通ることで固定する。
//
// **保証しない範囲（プロセスローカル）**: resumeAfter は永続化しない。パスの
// 周期より高頻度でプロセスが再起動する構成では C1 は成立しない --- ただし
// そのとき挙動は「毎パス先頭から」= 導入前と同じに戻るだけで、**悪化する経路は
// 無い**（未検証: 実際に高頻度再起動する構成での挙動は測っていない。上記は
// 再開位置がゼロ値に戻るという実装の性質からの推論である）。
//
// site 照合ガード（issue #139）は不要: EncodeReconcileArgs は site を持たず、
// mirakc にもファイルにも触れない（DB 読み + River Insert のみ）。どの site に
// 束縛された worker が拾っても結果は同じ。
type EncodeReconcileWorker struct {
	river.WorkerDefaults[jobs.EncodeReconcileArgs]
	Pool *pgxpool.Pool

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

	// resumeAfter は次のパスが候補を探し始める位置（この recording_id より
	// 大きい候補から見る。0 = 先頭から）。プロセスローカルで永続化しない
	// （上の doc コメント「窓を回す」参照）。ワーカーはプロセス生存期間中
	// 1 インスタンスが river.AddWorker に登録されて使い回されるので、この値は
	// パスをまたいで残る。atomic にしてあるのは、River がどの goroutine で
	// パスを実行するかに依存しないため（UniqueOpts が pending 中 1 本に
	// 合流させるので同時実行は起きないが、可視性の議論を残さない方が安い）。
	resumeAfter atomic.Int64
}

// Timeout は River の既定（1 分）より長い上限を与える。理由は
// encodeReconcileTimeout のコメントを参照。
func (w *EncodeReconcileWorker) Timeout(*river.Job[jobs.EncodeReconcileArgs]) time.Duration {
	return encodeReconcileTimeout
}

// Work は 1 パス分の encode reconcile を実行する。
//
// まず stale running encode の回収を行い、その後に候補の抽出と不足プロファイルの
// 判定を ListMissingEncodeProfiles でまとめて行う。
// これにより、候補ごとの原本・ポリシー・encoded の再取得を避ける。known_profiles
// も SQL に渡して、設定から消えたプロファイルや空のプロファイル名を投入対象から
// 外す。単発のヒント経路は用途が異なるため、引き続き EnqueueMissingEncodes 系の
// 実装を使う。
//
// 1 件の失敗でパス全体を止めない（record_sweep の processRecord・
// delete_reconcile の deleteMediaAsset と同じ判断）。次パスが同じ候補を
// 拾い直す。
func (w *EncodeReconcileWorker) Work(ctx context.Context, _ *river.Job[jobs.EncodeReconcileArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		// EncodeEnqueueHintWorker と同じ判断: このジョブの主目的が
		// 「encode ジョブを実際に投入すること」なので、client が取れないことを
		// 黙った no-op にすると取りこぼしの回復そのものが消える。
		return fmt.Errorf("encode reconcile: getting river client: %w", err)
	}

	// EncodeWorker.Timeout() は録画長に依存するため -1 のままにする。その代わり、
	// プロセス死で running のまま残った encode は job-id advisory lock の解放を
	// 確認して旧行を discarded にし、別 ID の代替ジョブへ置き換える。回収の失敗は
	// gap-fill（desired−observed の真実の再取得。不変条件 5）を止めない ---
	// 回収は補助経路で、次のパスが同じ候補を再び調べられるためである。
	if err := recoverStaleEncodeJobsFunc(ctx, w.Pool, client); err != nil {
		slog.Warn("encode_reconcile: recovering stale encode jobs failed, continuing with gap-fill", "err", err)
	}

	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = encodeReconcileRowLimit
	}
	known := w.Profiles.ProfileNames()

	// after は今パスが窓を開く位置（この recording_id より大きい候補から見る）。
	// resumeAfter はプロセスローカルなので、このワーカーインスタンスが前パスも
	// 実行していない（例: パスごとに新しいインスタンスを作った）場合は常に 0 に
	// 戻り、窓は回らない（EncodeReconcileWorker の doc コメント「窓を回す」参照）。
	after := w.resumeAfter.Load()

	q := sqlcgen.New(w.Pool)
	missing, err := q.ListMissingEncodeProfiles(ctx, sqlcgen.ListMissingEncodeProfilesParams{
		AfterRecordingID: after,
		KnownProfiles:    known,
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
	failed := 0
	for _, row := range missing {
		if len(candidates) == 0 || candidates[len(candidates)-1] != row.RecordingID {
			candidates = append(candidates, row.RecordingID)
		}
		if _, err := client.Insert(ctx, jobs.EncodeJobArgs{
			RecordingID: row.RecordingID,
			Profile:     row.Profile,
		}, nil); err != nil {
			failed++
			slog.Error("encode_reconcile: failed to enqueue missing encodes",
				"recording_id", row.RecordingID, "profile", row.Profile, "err", err)
		}
	}

	metrics.EncodeReconcileCandidates.Set(float64(len(candidates)))
	metrics.EncodeReconcileLastPass.SetToCurrentTime()

	// 窓を回す: ちょうど上限まで埋まったパスは続きが残っているかもしれないので
	// 最後に見た id から再開する。上限に届かなかった（0 件を含む）パスは候補集合の
	// 末尾まで見たので先頭へ戻す。1 件の投入失敗（上の failed）は再開位置に
	// 影響させない --- 巻き戻った後のパスでまた examine されるので、投入失敗の
	// ためだけの特別扱いは要らない。
	windowFull := int32(len(candidates)) >= rowLimit
	var resumeAfter int64
	if windowFull {
		resumeAfter = candidates[len(candidates)-1]
	}
	w.resumeAfter.Store(resumeAfter)

	// 窓が埋まったパスは、それより後ろの recording_id をこのパスでは見ていない。
	// 次パスが resume_after から続きを見る（黙って終わらせない。上の doc コメント
	// 参照）。resume_after は回転が実際に進んでいることを運用側から確かめる
	// 唯一の手段（プロセスが再起動を繰り返す構成では常に 0 に留まり、それも
	// ここに現れる）。
	if windowFull {
		slog.Warn("encode_reconcile: candidate window is full; the next pass resumes from resume_after",
			"row_limit", rowLimit, "last_recording_id", candidates[len(candidates)-1], "resume_after", resumeAfter)
	}

	w.reportUnsatisfiable(ctx, q, known)

	if len(candidates) > 0 || failed > 0 {
		slog.Info("encode_reconcile: pass complete",
			"candidates", len(candidates), "failed", failed, "row_limit", rowLimit, "resume_after", resumeAfter)
	}
	return nil
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
