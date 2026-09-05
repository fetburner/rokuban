package worker

import (
	"context"
	"sync"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// OnceOutcome は 1 件消化モード（OnceGate）が待機を終えた理由。
//
// **どの理由でも呼び出し側は exit 0 にする**（ジョブの失敗は River の
// リトライが引き取るので、プロセスの終了コードで表さない。ClientConfig.Once
// のコメント参照）。この値は「なぜ終わったか」をログに残すためだけにある ---
// 「1 件やって終わった」と「空振りで終わった」を運用側で区別できないと、
// KEDA が Job を起こし続けているのに仕事が進まない状態を見分けられない。
type OnceOutcome int

const (
	// OnceOutcomeJobDone は 1 件のジョブが Work を抜けたことを表す
	// （成功・失敗のどちらも含む）。
	OnceOutcomeJobDone OnceOutcome = iota

	// OnceOutcomeIdleTimeout はジョブを 1 件も claim しないまま待ち時間を
	// 使い切ったことを表す。KEDA が滞留を過大評価して Job を起こしすぎた
	// （overshoot した）ときにこちらへ落ちる。
	OnceOutcomeIdleTimeout

	// OnceOutcomeCanceled は ctx が終了したことを表す（SIGTERM / ノード退避）。
	OnceOutcomeCanceled

	// OnceOutcomeJobUnhandled はジョブが終端に達したが **worker に入らなかった**
	// ことを表す。River に登録されていない kind のジョブを掴んだ場合に起きる
	// （SubscribeOnceEvents のコメント）。
	//
	// job_done と分けているのは、これが「仕事をした」ではなく「掴んで失敗させ、
	// 試行回数を 1 つ潰した」だからである。CronJob や api が新しいイメージで、
	// worker が古いイメージという版ずれで起きうるので、KEDA が Job を起こし
	// 続けているのに何も進まない状態をログで名指しできるようにする。
	OnceOutcomeJobUnhandled
)

// String は OnceOutcome のログ用の名前を返す。
func (o OnceOutcome) String() string {
	switch o {
	case OnceOutcomeJobDone:
		return "job_done"
	case OnceOutcomeIdleTimeout:
		return "idle_timeout"
	case OnceOutcomeCanceled:
		return "canceled"
	case OnceOutcomeJobUnhandled:
		return "job_unhandled"
	default:
		return "unknown"
	}
}

// OnceGate は「ジョブ 1 件を消化したら終了する」プロセスの終了条件を作る。
//
// KEDA ScaledJob はキューアイテムごとに k8s Job を起こし、**その Job が自分で
// 終了すること**を前提にした機構である（docs/operations.md §5「worker: KEDA
// ScaledJob」）。`rokuban server --roles worker` は本来終了しないので、
// 1 件消化モード（`--once`）ではこの Gate が終了の契機を作る。
//
// 仕組みは 2 つのチャネルだけ:
//
//   - Middleware が返す River の WorkerMiddleware が、最初のジョブの Work の
//     入口で started を、出口で done を閉じる
//   - Wait が done（消化した）・タイムアウト（空振り）・ctx 終了のいずれかを待つ
//
// **タイムアウトは claim 前にしか効かない。** 実行中のジョブを打ち切らない
// ことが ScaledJob を選んだ理由そのもの（「ジョブは完走するまで殺されない」）
// なので、タイムアウトは「起きたが掴む仕事が無かった Job」を終わらせるためだけ
// に使う。数時間のエンコードはタイムアウトの対象にならない。
type OnceGate struct {
	startOnce sync.Once
	doneOnce  sync.Once
	started   chan struct{}
	done      chan struct{}
}

// NewOnceGate は OnceGate を作る。
func NewOnceGate() *OnceGate {
	return &OnceGate{
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Middleware は River に登録する WorkerMiddleware を返す。
//
// **doInner のエラーをそのまま返す。** 握り潰すと River はそのジョブを
// completed として確定させてしまい、失敗したジョブが二度と再試行されない
// （at-least-once が壊れる）。プロセスの終了コードを 0 にするのは呼び出し側の
// 責任であって、ここでエラーを消すことではない
// （TestOnceGate_MiddlewarePropagatesWorkerError）。
func (g *OnceGate) Middleware() rivertype.Middleware {
	return river.WorkerMiddlewareFunc(func(ctx context.Context, _ *rivertype.JobRow, doInner func(context.Context) error) error {
		g.startOnce.Do(func() { close(g.started) })
		defer g.doneOnce.Do(func() { close(g.done) })
		return doInner(ctx)
	})
}

// Wait は次のいずれかが起きるまでブロックし、その理由を返す。
//
//   - 最初のジョブが Work を抜けた → OnceOutcomeJobDone
//   - ジョブが worker に入らずに終端に達した → OnceOutcomeJobUnhandled
//   - ジョブを 1 件も claim しないまま idleTimeout が経過 → OnceOutcomeIdleTimeout
//   - ctx が終了した → OnceOutcomeCanceled
//
// events は SubscribeOnceEvents が返すチャネル（nil でもよい。その場合は
// middleware から観測できるジョブだけが終了の契機になる）。
//
// **claim 済みならタイムアウトを適用しない**（型の説明参照）。
func (g *OnceGate) Wait(ctx context.Context, idleTimeout time.Duration, events <-chan *river.Event) OnceOutcome {
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	return g.wait(ctx, timer.C, events)
}

// wait は Wait の本体。
//
// **タイムアウトのチャネルを引数で受けるのはテストのため。** 「タイマーが先に
// 発火した状態で、なおジョブが実行中」という組み合わせは実時間のタイマーでは
// 決定的にスケジュールできない（Wait 経由だと select 評価の時点でタイマーが
// まだ発火していないので、この分岐に入らない）。発火済みのチャネルを渡せる
// ようにして、下の再確認が実際に効いていることを
// TestOnceGate_IdleTimeoutDoesNotCutRunningJob が固定する。
func (g *OnceGate) wait(ctx context.Context, timeout <-chan time.Time, events <-chan *river.Event) OnceOutcome {
	for {
		select {
		case <-ctx.Done():
			return OnceOutcomeCanceled
		case <-g.done:
			return OnceOutcomeJobDone
		case _, ok := <-events:
			if !ok {
				// **購読が閉じたことを「ジョブが終わった」と読まない。** 閉じた
				// チャネルは永久に受信可能なので、読み替えると Job が即座に
				// 終了し、しかも outcome がでっち上げ（job_unhandled）になる
				// （実測: 閉じたチャネルを渡すと job_unhandled が即返る）。
				// River は unsubscribe と購読マネージャの停止で閉じる。
				// 第 2 の観測点を失っただけなので、middleware の観測だけで待ち続ける。
				events = nil
				continue
			}
			// **middleware を通らずに終わったジョブ。** 未登録 kind がこれに当たる
			// （SubscribeOnceEvents のコメント）。ここで終わらせないと、Job は
			// idleTimeout の間そのキューを掴み続けて試行回数を潰す。
			//
			// started を見直すのは、worked なジョブの done と event が同時に
			// 準備できたときに job_unhandled と誤報しないため（select は
			// ランダムに選ぶ）。
			select {
			case <-g.started:
				return g.waitDone(ctx, events)
			default:
				return OnceOutcomeJobUnhandled
			}
		case <-g.started:
			return g.waitDone(ctx, events)
		case <-timeout:
			// **タイマーと claim が同時に成立した場合は claim を優先する。**
			// select は準備できた case をランダムに選ぶので、ここで started を
			// 見直さないと、実行中のジョブを抱えたまま idle_timeout を返す ---
			// 呼び出し側はそれを「空振りだった」と読む（打ち切り自体は呼び出し側の
			// graceful stop が防ぐ。cmd/rokuban/server.go の once gate 参照）。
			select {
			case <-g.started:
				return g.waitDone(ctx, events)
			default:
				return OnceOutcomeIdleTimeout
			}
		}
	}
}

// waitDone は claim 済みのジョブが Work を抜けるまで待つ（タイムアウトなし）。
func (g *OnceGate) waitDone(ctx context.Context, events <-chan *river.Event) OnceOutcome {
	for {
		select {
		case <-ctx.Done():
			return OnceOutcomeCanceled
		case <-g.done:
			return OnceOutcomeJobDone
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			return OnceOutcomeJobDone
		}
	}
}

// SubscribeOnceEvents は 1 件消化モードが「ジョブが executor を出た」ことを
// 観測するための購読を張る。返す関数で購読を解除する。
//
// **middleware だけでは足りない。** River の executor は、登録されていない
// kind のジョブに対して WorkUnit == nil で早期 return し、その時点では
// **WorkerMiddleware のチェーンをまだ組み立てていない**
// （river@v0.47.0 internal/jobexecutor/job_executor.go の `if e.WorkUnit == nil`
// が `execution.MiddlewareChain` より前にある）。したがって OnceGate の
// middleware は一度も呼ばれず、Job は「1 件も claim していない」と誤認したまま
// idleTimeout まで掴み続ける。
//
// 逆に Subscribe だけでも足りない。終端イベントは「終わった」しか伝えないので、
// 「まだ掴んでいない」と「掴んで実行中」を区別できず、実行中のジョブを
// idleTimeout で打ち切ることになる。2 つの観測点が両方要る。
//
// **ただし本番の配線（buildRiverConfig。cfg.Once != nil なら常に
// OnceGate.Middleware() も一緒に登録する）では、4 つの購読のうち実際に
// 効いているのは EventKindJobFailed だけである。** 未登録 kind のジョブは
// Work を一度も呼ばれないので、起こしようがあるのは job_failed だけ
// （completed/cancelled/snoozed は Work が明示的に選ぶ終わり方であり、
// 呼ばれてすらいないジョブには起きない）。一方、登録済み kind は
// WorkUnit != nil なので早期 return せず必ず execution.MiddlewareChain を
// 組み立てる ---
// つまりどの終端でも WorkerMiddleware は必ず呼ばれ、g.done が先に閉じて
// Job を終わらせる。completed はこれを実測済み --- EventKindJobCompleted を
// 購読から落としても TestServerCmd_OnceModeTerminates の「1 件あれば消化して
// 終了する」は 0.19s で変わらず緑だった。cancelled/snoozed はこの実測を
// 持たない（本番配線で snooze/cancel を起こす --once のテストが無い）。
// job_executor.go の読解（上記）からはこの 2 つも同じ結論になるはずだが、
// **未検証**。completed/cancelled/snoozed の 3 つは「将来 River が
// middleware チェーンの組み立てタイミングを変える」場合への保険であって、
// 今の River (v0.47.0) ではこの 3 kind を落としても本番の挙動は壊れない
// と考えられる（根拠の強さは上記の通り一様ではない）。
//
// **River v0.44 で 5 つ目の終わり方（EventKindJobInterrupted）が増えたが、
// これは購読しない。** クライアントの停止で実行中のジョブが打ち切られた場合
// （`SoftStopTimeout` 切れ）にだけ起き、行は attempt を消費せず available に
// 戻る（river@v0.47.0 event.go の `EventKindJobInterrupted`）。1 件消化モードでこれが起きるのは
// **停止が始まったあと**、つまり Start に渡した ctx が cancel されたか
// stopOnceProcess が Stop を撃ったあとだけである。前者では Wait が
// ctx.Done で OnceOutcomeCanceled を返し、後者では Wait が既に戻っている。
// どちらでも購読は待ちの契機として要らない。**購読すると害がある側でもない**
// （Wait は最初の 1 件で戻るだけ）が、「終端」の意味を停止と混ぜないために
// 4 kind に留める。
//
// 未登録 kind の検出（本番で唯一効いている経路）は
// cmd/rokuban.TestServerCmd_OnceModeExitsOnUnhandledJobKind が本番と同じ
// 配線（RunE 経由）で固定している --- idleTimeout を待たずに終わることに
// 加え、試行回数を 1 回しか潰していないことも見ている。
// once_river_test.go の TestSubscribeOnceEvents_AllTerminalKindsEndWaitViaEventsWithoutMiddleware
// は、middleware をあえて外した非本番の配線で 4 kind それぞれが events
// だけで Wait を終わらせられることを確認する（上記の保険側の裏付け。
// go.mod の river の版が上がってここが崩れても go build/go vet では
// 検出できない）。
func SubscribeOnceEvents(client *river.Client[pgx5.Tx]) (<-chan *river.Event, func()) {
	// 4 種すべてを取る。本番で唯一効くのは job_failed（未登録 kind の
	// 唯一の終わり方）で、残り 3 つは将来 River が middleware の組み立て
	// タイミングを変えた場合への保険（このコメントの上の説明参照）。
	// snooze は「進捗ゼロで Job が終わる」形になるが、行は available を
	// 離れるので KEDA が同じジョブで Job を起こし続けることはない。
	return client.Subscribe(
		river.EventKindJobCompleted,
		river.EventKindJobFailed,
		river.EventKindJobCancelled,
		river.EventKindJobSnoozed,
	)
}
