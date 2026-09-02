package worker

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reconciler"
)

const (
	// reconcilerQueue は reconcile_pass ジョブ専用のキュー名。
	reconcilerQueue = "reconciler"

	// defaultReconcilePassInterval は定期パスの既定間隔。従来の reconciler の
	// 自前タイマー（reconciler.defaultConfig の ReconcileInterval）が使っていた
	// 30 秒をそのまま引き継ぐ（docs/recording.md §3.2「定期（既定 30 秒）」）。
	defaultReconcilePassInterval = 30 * time.Second

	// reconcilePassTimeout は 1 パス（存在の突き合わせ + オプション差分反映 + GC 相当の
	// never-scheduled 記録）全体の上限。
	//
	// ruler（rulerPassTimeout、5 分）は mirakc に一切触れない純粋な Postgres 処理だが、
	// reconciler は mirakc への HTTP を伴う: schedules の全量 GET が 1 回、加えて
	// desired 集合との差分ぶんの POST（作成）/ DELETE（削除。ただし全損シグネチャの
	// サーキットブレーカーが発動中はゼロになる）が観測対象の件数に比例して発生する。
	// ネットワーク往復が支配的になりうる点は
	// epg_sync（mirakc への GET 2 回 + 応答の Postgres 投影）と同じ性質なので、
	// epg_sync と同じ既定より長い上限を与える。ingest（Timeout() が -1）とは異なり、
	// 1 回の HTTP 呼び出しが数百 MB を転送するわけではなく、mirakc 側の応答が
	// 返らない限りいつまでも待ち続ける理由がないため、無制限にはしない。
	reconcilePassTimeout = 10 * time.Minute
)

// ReconcilePassArgs は reconciler の 1 パス突き合わせジョブの引数。
//
// サイト単位でジョブを分けるのは、reconciler の排他がジョブロック + UniqueOpts
// （サイト単位）で行われるため（docs/data.md §2「ruler と reconciler はシングルトン
// ではなくジョブ」）。別サイトの並行実行は正常。
type ReconcilePassArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (ReconcilePassArgs) Kind() string { return "reconcile_pass" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// UniqueOpts{ByArgs, ByState} でサイト単位に排他する。同時実行の防止だけでなく、
// 起動契機のヒント（予約の作成/取消・ruler パスの完了）を定期実行に合流させる機構
// でもある（docs/recording.md §3.2「ヒントは UniqueOpts{ByArgs, ByState} で合流する」）。
// ByState は pendingJobStates に絞る。既定（completed を含む）のままだと、一度
// 成功したサイトの引数は二度と投入できず、定期ジョブが実質ワンショットになる
// （ruler_pass / epg_sync と同じ理由）。
//
// Queue は a.Site で修飾する（physicalQueueName、issue #185 M4-13。必ず
// physicalQueueName を経由する --- qualifyQueueName のコメント参照）。
// reconciler は mirakc への到達性を要する site 単位の仕事なので、多サイト構成で
// 他サイトの worker が掴まないよう、キュー選択の時点で分離する。
//
// ByQueue: uniqueByQueue の理由は pendingJobStates 直後の doc コメント参照。
func (a ReconcilePassArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: physicalQueueName(reconcilerQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// ReconcilePassWorker は reconciler の 1 パス突き合わせを実行する River ワーカー。
//
// ロジックは internal/reconciler にそのまま置いてあり、ここでは呼び出すだけ
// （ロジックの移植はしない）。reconciler は mirakc への HTTP を伴うため、ruler と
// 違って MirakcClients を依存として持つ。
type ReconcilePassWorker struct {
	river.WorkerDefaults[ReconcilePassArgs]

	// MirakcClients は site → mirakc クライアントの map（issue #532）。この
	// 1 インスタンスが複数 site の reconciler_<site> キューを同時に購読しうる。
	// Work は verifySite で job.Args.Site に対応するクライアントを取り出してから
	// reconciler.New に渡す（issue #139）。
	MirakcClients map[string]*mirakc.Client
	Pool          *pgxpool.Pool

	// StartDelayGrace は開始遅延検出器の猶予（reconciler.Config.StartDelayGrace）。
	// 0 なら reconciler 側の既定値を使う（config.yml の
	// reconciler.start_delay_grace から注入される）。
	StartDelayGrace time.Duration
}

// Timeout は River の既定（1 分）より長い上限を与える。理由は reconcilePassTimeout の
// コメントを参照。
func (w *ReconcilePassWorker) Timeout(*river.Job[ReconcilePassArgs]) time.Duration {
	return reconcilePassTimeout
}

// Work は reconciler の 1 パス突き合わせを実行する。対象サイトはジョブ引数の 1 サイトのみ
// （ジョブの排他がサイト単位のため）。
//
// reconciler.Reconciler 自身はパスを跨ぐ状態を持たないため、ジョブごとに毎回
// 新規に生成してよい（issue #24 M2-17）。大量削除サーキットブレーカー
// （breaker.ReconcileTotalLoss）は例外で、発動のラッチは Reconciler 構造体ではなく
// circuit_breakers テーブルに永続化される（issue #24 M2-5）。RunPass はパスの
// 先頭でその状態を DB から読み直すので、ここで毎回新規生成しても発動状態は
// 失われない。
func (w *ReconcilePassWorker) Work(ctx context.Context, job *river.Job[ReconcilePassArgs]) error {
	// mirakc インスタンスはサイトスコープ。他サイトのジョブをこのプロセスの
	// mirakc に投げると、別インスタンスの schedules をこのサイトの予約として
	// 作成/削除しうる（issue #139）。reconciler.New/RunPass より前に照合する。
	client, err := verifySite(w.MirakcClients, job.Args.Site, reconcilerQueue)
	if err != nil {
		return err
	}

	rec := reconciler.New(job.Args.Site, client, w.Pool, &reconciler.Config{
		StartDelayGrace: w.StartDelayGrace,
	})
	return rec.RunPass(ctx)
}
