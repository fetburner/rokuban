package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

const (
	// defaultEncodeReconcileInterval は encode の desired−observed 定期パスの
	// 既定間隔。
	//
	// このパスが埋めるのはヒント経路（ingest 完了時のベストエフォート投入と
	// POST /api/recordings/{id}/encode-profiles のヒントジョブ）が両方とも
	// 落ちたときの取りこぼしだけなので、通常運転では毎パス 0 件になる。
	// 秒単位の応答性は要らない一方、取りこぼしを何時間も放置したくもないので、
	// 同じく「普段は何も起きないバックストップ」である delete_reconcile と
	// 同じ 15 分に揃える。
	defaultEncodeReconcileInterval = 15 * time.Minute

	// encodeReconcileTimeout は 1 パス全体の上限。
	//
	// River の既定（1 分）より長く与える: 候補 1 件ごとに EnqueueMissingEncodes が
	// 原本の確認・ポリシーの読み出し・プロファイルごとの encoded 確認と、
	// 不足分の Insert を行うため、候補数（最大 encodeReconcileRowLimit）に比例して
	// DB のラウンドトリップが積み上がる。ffmpeg は一切起動しない（不変条件 4 に
	// 触れない。投入するだけ）ので、encode ジョブ本体（Timeout() が -1）のような
	// 無制限は要らない。
	encodeReconcileTimeout = 5 * time.Minute

	// encodeReconcileRowLimit は 1 パスで拾う候補録画の上限。
	//
	// 際限なく積み上げて encodeReconcileTimeout を食い潰すのを避けるための
	// 安全弁（delete_reconcileRowLimit と同じ役割）。打ち切っても次パスが
	// 同じ候補を先頭から拾い直す（ORDER BY recording_id）ので、レベルトリガー
	// としての収束性は失われない。
	encodeReconcileRowLimit = 1000
)

// EncodeReconcileArgs は encode の desired−observed 定期 reconcile ジョブの引数
// （issue #163）。
//
// # 専用の定期ジョブにした理由（record_sweep に相乗りしなかった理由）
//
// record_sweep（watcher の定期全量突き合わせ。docs/recording/watcher.md §3.3 の
// (c)）は「mirakc のエッジに残っている record」を真実として引き直すパスで、
// 次の 3 点がこのパスと合わない:
//
//  1. **site 束縛**: record_sweep は mirakc への到達性を要するので site 単位の
//     キュー（`watcher_<site>`）に乗り、verifySite で自 site のジョブしか
//     処理しない。一方エンコードは site の属性を持たない（アーカイブも
//     プロファイルも単一。EncodeWorker の doc コメント参照）。相乗りさせると、
//     site に属さない 1 つの懸念を site ごとに N 回評価することになり、
//     どの site の worker が拾うかで結果が変わらないのに N 倍の全表走査が走る。
//  2. **対象集合**: record_sweep が見るのはエッジの record（DB の外の観測）で、
//     こちらは「原本コミット済みなのに encoded が揃っていない録画」（DB だけで
//     閉じる）。エッジから record が消えた後こそこのパスの出番なので、
//     record_sweep の走査対象からはそもそも見えない（issue #163 の「罠」が
//     クエリを共有するなと言っているのはこのため）。
//  3. **依存の向き**: record_sweep の本体は internal/watcher にあり、
//     internal/worker に依存できない（循環インポート。RecordSweepWorker の
//     doc コメント参照）ため、EnqueueMissingEncodes を呼ぶには注入が要る。
//
// 引数は空（site を持たない）。DeleteReconcileArgs / CatalogExportArgs /
// StorageSyncArgs と同じく、対象が site に従属しない資源だけだからである。
type EncodeReconcileArgs struct{}

// Kind は River ジョブの種別名を返す。
func (EncodeReconcileArgs) Kind() string { return "encode_reconcile" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// # キューを encode にした理由と、その代償
//
// 仕事の中身が EncodeEnqueueHintWorker と同じ（DB を読んで encode ジョブを
// Insert するだけ。ffmpeg は起動しない）なので、同じ encode キューに置く。
// これにより `worker.queues` で encode を外した Pod からは「エンコードを
// 実行する側」と「エンコードを投入する側」が一緒に外れ、購読集合の意味が
// 「エンコードに関わるか」の 1 軸で済む。cleanup は「物理削除系ジョブ専用」
// （cleanupQueue のコメント）、storage は観測用と名付けが決まっているので、
// どちらにも混ぜない。
//
// 代償: River のキュー単位の MaxWorkers はジョブ種を区別しない
// （river@v0.40.0/client.go:634「MaxWorkers is the maximum number of workers to
// run for the queue」）ため、`encode.concurrency: 1`（既定）の構成では実行中の
// encode ジョブが終わるまでこのパスは走らない。許容する: エンコードが詰まって
// いる系では今すぐ投入しても実行されないので、検出が遅れても失うものが無い。
// 待っている間にパスが積み上がることもない（下記 UniqueOpts で pending 中の
// 1 本に合流する）。
//
// # 二重投入の防止
//
// UniqueOpts{ByArgs, ByState: pendingJobStates} で「pending 中のパスは 1 本」に
// する。ByArgs は付けても付けなくてもよい（Args が空なので鍵は kind だけで
// 決まる）が、他の定期ジョブと同じ形にして「引数が増えたときに一意性が
// 壊れる」経路を作らない。ByQueue は付けない --- encode キューは site 修飾の
// 対象外（siteBoundQueueNames に無い）で、キュー名の変更予定も無いため
// （uniqueByQueue のコメント「対象外のキューまで変更すると説明が必要になる
// 範囲が広がるだけ」）。
//
// パスが投入する encode ジョブ側の二重投入は EncodeJobArgs.InsertOpts の
// UniqueOpts が防ぐ。実測（TestEncodeReconcile_DoesNotDoubleEnqueue）: 同じ
// (recording_id, profile) の Insert は 2 回目以降
// `UniqueSkippedAsDuplicate = true` で同一のジョブ ID を返し、river_job の
// 行は 1 行のまま増えない。
func (EncodeReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: encodeQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// EncodeReconcileWorker は desired（recording_encode_policy.encode_profiles）−
// observed（active な encoded media_assets）の差分を定期的に埋める River ワーカー
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
// site 照合ガード（issue #139）は不要: EncodeReconcileArgs は site を持たず、
// mirakc にもファイルにも触れない（DB 読み + River Insert のみ）。どの site に
// 束縛された worker が拾っても結果は同じ。
type EncodeReconcileWorker struct {
	river.WorkerDefaults[EncodeReconcileArgs]
	Pool *pgxpool.Pool
}

// Timeout は River の既定（1 分）より長い上限を与える。理由は
// encodeReconcileTimeout のコメントを参照。
func (w *EncodeReconcileWorker) Timeout(*river.Job[EncodeReconcileArgs]) time.Duration {
	return encodeReconcileTimeout
}

// Work は 1 パス分の encode reconcile を実行する。
//
// 候補の抽出（ListRecordingsMissingEncodes）と実際の投入判断
// （EnqueueMissingEncodes）を分けてある。候補クエリは「1 件も差分が無い録画を
// 1 件ずつ舐めない」ための絞り込みであって、投入するかどうかの判断そのものは
// 常に EnqueueMissingEncodes 側にある（ヒント経路と同じ関数を通す。判断が
// 2 か所に分かれると片方だけ直る）。
//
// 1 件の失敗でパス全体を止めない（record_sweep の processRecord・
// delete_reconcile の deleteMediaAsset と同じ判断）。次パスが同じ候補を
// 拾い直す。
func (w *EncodeReconcileWorker) Work(ctx context.Context, _ *river.Job[EncodeReconcileArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		// EncodeEnqueueHintWorker と同じ判断: このジョブの主目的が
		// 「encode ジョブを実際に投入すること」なので、client が取れないことを
		// 黙った no-op にすると取りこぼしの回復そのものが消える。
		return fmt.Errorf("encode reconcile: getting river client: %w", err)
	}

	q := sqlcgen.New(w.Pool)
	candidates, err := q.ListRecordingsMissingEncodes(ctx, encodeReconcileRowLimit)
	if err != nil {
		return fmt.Errorf("listing recordings missing encodes: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	failed := 0
	for _, recordingID := range candidates {
		if err := EnqueueMissingEncodes(ctx, client, w.Pool, recordingID); err != nil {
			failed++
			slog.Error("encode_reconcile: failed to enqueue missing encodes",
				"recording_id", recordingID, "err", err)
		}
	}

	// 通常運転では候補は 0 件（ヒントが届いていれば encoded が揃っているか
	// encode ジョブが pending 中で、いずれこの候補集合から外れる）。0 件でない
	// パスは「ヒントが落ちた」か「encode がまだ終わっていない」のどちらかなので、
	// 件数を残す。
	slog.Info("encode_reconcile: pass complete",
		"candidates", len(candidates), "failed", failed, "row_limit", encodeReconcileRowLimit)
	return nil
}
