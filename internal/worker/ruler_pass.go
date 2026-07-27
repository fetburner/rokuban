package worker

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/ruler"
)

const (
	// rulerQueue は ruler_pass ジョブ専用のキュー名。
	rulerQueue = "ruler"

	// defaultRulerPassInterval は定期パスの既定間隔。docs/recording.md §3.1 の
	// 起動契機の表で「定期（既定 10 分）」としている値と揃える。
	defaultRulerPassInterval = 10 * time.Minute

	// rulerPassTimeout は 1 パス（全ルール評価 + GC）全体の上限。
	//
	// ingest（Timeout() が -1）とは事情が異なる。ingest は mirakc からの数百 MB〜
	// 数十 GB のバイト転送で、所要時間が録画長・回線速度という外部要因に支配される
	// ため無制限にせざるを得なかった。ruler は mirakc に一切触れない（不変条件 1）
	// 純粋な Postgres 処理で、外部ネットワーク転送のように所要時間が無制限に伸びる
	// 要因がないため、無制限にする理由がない。
	//
	// 一方で既定の 1 分（river.JobTimeoutDefault）は窮屈になりうる。評価そのものは
	// 秒未満（docs/recording.md §3.1「規模は問題にならない」）だが、現在の実装は
	// ルールごとに 1 回 DB ラウンドトリップを行う（rulequery.MatchProgramIDsForRule）
	// ため、「数十〜数百ルール」（同 §3.1）に増えるとラウンドトリップの積み重ねで
	// 既定を超えうる。同一パス内で番組終了後の GC（runGC）も走る。epg_sync が既定を
	// 超える 10 分を与えているのと同じ検討に基づき、既定より余裕を持たせた値を置く。
	rulerPassTimeout = 5 * time.Minute
)

// RulerPassArgs は ruler の 1 パス評価ジョブの引数。
//
// サイト単位でジョブを分けるのは、ruler の排他がジョブロック + UniqueOpts
// （サイト単位）で行われるため（docs/data.md §2「ルール評価（ruler）はシングルトン
// ではなくジョブ」）。別サイトの並行実行は正常。
type RulerPassArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (RulerPassArgs) Kind() string { return "ruler_pass" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// UniqueOpts{ByArgs, ByState} でサイト単位に排他する。これは同時実行の防止だけでなく、
// 起動契機のヒント（ルール編集・EPG 同期完了）を定期実行に合流させる機構でもある
// （docs/recording.md §3.1「ヒントは UniqueOpts{ByArgs, ByState} で合流する」）。
// ByState は pendingJobStates に絞る。既定（completed を含む）のままだと、一度
// 成功したサイトの引数は二度と投入できず、定期ジョブが実質ワンショットになる
// （epg_sync と同じ理由。InsertOpts のコメント参照）。
func (RulerPassArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: rulerQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// RulerPassWorker は ruler の 1 パス評価を実行する River ワーカー。
//
// ロジックは internal/ruler にそのまま置いてあり、ここでは呼び出すだけ
// （ロジックの移植はしない）。ruler は mirakc に触れない（不変条件 1）ので、
// このワーカーが持つ依存は Pool と RetentionGrace だけで足りる。
type RulerPassWorker struct {
	river.WorkerDefaults[RulerPassArgs]
	Pool *pgxpool.Pool

	// RetentionGrace は番組終了後、reservations / program_intents を GC するまでの
	// 猶予。0 なら ruler 側の既定値を使う（epg.retention_grace をそのまま流用する
	// 運用を想定。ruler.Config.RetentionGrace のコメント参照）。
	RetentionGrace time.Duration
}

// Timeout は River の既定（1 分）より長い上限を与える。理由は rulerPassTimeout の
// コメントを参照。
func (w *RulerPassWorker) Timeout(*river.Job[RulerPassArgs]) time.Duration {
	return rulerPassTimeout
}

// Work は ruler の 1 パス評価を実行する。対象サイトはジョブ引数の 1 サイトのみ
// （ruler.New はサイトのスライスを受け取れるが、ジョブの排他がサイト単位のため
// ここでは常に長さ 1 で渡す）。
func (w *RulerPassWorker) Work(ctx context.Context, job *river.Job[RulerPassArgs]) error {
	r := ruler.New([]string{job.Args.Site}, w.Pool, &ruler.Config{
		RetentionGrace: w.RetentionGrace,
	})
	return r.RunPass(ctx)
}
