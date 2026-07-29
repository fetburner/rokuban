package worker

import (
	"context"
	"fmt"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/watcher"
	"github.com/fetburner/rokuban/internal/webhook"
)

const (
	// recordSweepQueue は record_sweep ジョブ専用のキュー名。
	//
	// watcher（SSE 常駐）の名を借りる: このジョブは watcher の 3 段構え
	// （docs/recording.md §3.3）のうち (c) 定期全量突き合わせを切り出したもので、
	// ruler / reconciler と同じキュー命名規則（担当ドメイン名）に揃える。
	recordSweepQueue = "watcher"

	// defaultRecordSweepInterval は定期パスの既定間隔。従来の watcher の自前タイマー
	// （旧 watcher.defaultConfig の ReconcileInterval）が使っていた 5 分をそのまま
	// 引き継ぐ（docs/recording.md §3.3）。
	defaultRecordSweepInterval = 5 * time.Minute

	// recordSweepTimeout は 1 パス（`GET /api/recording/records` の全量取得 + record
	// ごとの processRecord）全体の上限。
	//
	// reconciler（reconcilePassTimeout、10 分）と同じ理由で river の既定（1 分）より
	// 長く与える: mirakc への HTTP（records の全量 GET 1 回）に加え、record ごとに
	// DB トランザクションが 1 回発生する（record_sync の行ロック確保 + recordings
	// upsert 相当）ため、record 件数に比例して往復コストが積み上がる。
	// ingest（Timeout() が -1）とは異なり、record のバイト転送そのものはこのジョブに
	// 含まれない（ストリーム転送は別の ingest ジョブが担う）ため、無制限にはしない。
	recordSweepTimeout = 10 * time.Minute
)

// RecordSweepArgs は watcher の定期全量突き合わせ（docs/recording.md §3.3 の (c)）
// ジョブの引数。
//
// サイト単位でジョブを分けるのは ruler / reconciler と同じ理由（ジョブロック +
// UniqueOpts をサイト単位で行うため。docs/data.md §2）。別サイトの並行実行は正常。
type RecordSweepArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (RecordSweepArgs) Kind() string { return "record_sweep" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// UniqueOpts{ByArgs, ByState} でサイト単位に排他する。同時実行の防止に加え、
// 定期実行のヒントを合流させる機構でもある（ruler_pass / reconcile_pass と同じ形）。
// ByState は pendingJobStates に絞る。理由は ReconcilePassArgs.InsertOpts のコメント
// 参照（既定のまま completed を含めると定期ジョブが実質ワンショットになる）。
func (RecordSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: recordSweepQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// RecordSweepWorker は watcher の定期全量突き合わせ（(c)）を実行する River ワーカー。
//
// ロジックは internal/watcher の Watcher.Sweep にそのまま置いてあり、ここでは
// 呼び出すだけ（ロジックの移植はしない）。M2-16 で processRecord が
// record_sync の (site, record_id) 行ロックにより冪等化されているため、SSE 由来の
// 処理（(a)(b)、Watcher.Run の常駐側）とこのジョブ（(c)）が同一 record を並行処理
// しても recordings は重複しない（docs/recording.md §3.3「record 処理は並行実行
// しても壊れない」）。
//
// internal/watcher は internal/worker に依存できない（依存すると本ワーカーが
// watcher パッケージを import する関係と合わせて循環インポートになる）ため、
// ingest ジョブ引数は NewIngestArgs を watcher.IngestArgsFunc として注入する。
type RecordSweepWorker struct {
	river.WorkerDefaults[RecordSweepArgs]
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	// Webhook は processRecord 経由の finished 遷移通知に使う（M3-11）。nil 可。
	Webhook *webhook.Client
}

// Timeout は River の既定（1 分）より長い上限を与える。理由は recordSweepTimeout の
// コメントを参照。
func (w *RecordSweepWorker) Timeout(*river.Job[RecordSweepArgs]) time.Duration {
	return recordSweepTimeout
}

// Work は watcher の定期全量突き合わせを実行する。対象サイトはジョブ引数の 1 サイトのみ
// （ジョブの排他がサイト単位のため）。
//
// Watcher は ingest ジョブを同一トランザクションで InsertTx するため、実行中の
// River クライアントを渡す必要がある。Deps に river.Client を持たせると
// NewWorkers（クライアント生成前に呼ばれる）で鶏と卵になるため、ruler_pass の
// ヒント投入と同じ手法（river.ClientFromContextSafely）でジョブ実行コンテキストから
// 取り出す。Worker の Work() 内では必ず取得できる（River のドキュメント通り、
// ここで失敗することはない）。
func (w *RecordSweepWorker) Work(ctx context.Context, job *river.Job[RecordSweepArgs]) error {
	riverClient, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("getting river client from job context: %w", err)
	}

	wt := watcher.New(job.Args.Site, w.MirakcClient, w.Pool, riverClient, NewIngestArgs, w.Webhook)
	return wt.Sweep(ctx)
}
