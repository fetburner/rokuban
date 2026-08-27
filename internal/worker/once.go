package worker

import (
	"context"
	"sync"
	"time"

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
//   - ジョブを 1 件も claim しないまま idleTimeout が経過 → OnceOutcomeIdleTimeout
//   - ctx が終了した → OnceOutcomeCanceled
//
// **claim 済みならタイムアウトを適用しない**（型の説明参照）。
func (g *OnceGate) Wait(ctx context.Context, idleTimeout time.Duration) OnceOutcome {
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	return g.wait(ctx, timer.C)
}

// wait は Wait の本体。
//
// **タイムアウトのチャネルを引数で受けるのはテストのため。** 「タイマーが先に
// 発火した状態で、なおジョブが実行中」という組み合わせは実時間のタイマーでは
// 決定的にスケジュールできない（Wait 経由だと select 評価の時点でタイマーが
// まだ発火していないので、この分岐に入らない）。発火済みのチャネルを渡せる
// ようにして、下の再確認が実際に効いていることを
// TestOnceGate_IdleTimeoutDoesNotCutRunningJob が固定する。
func (g *OnceGate) wait(ctx context.Context, timeout <-chan time.Time) OnceOutcome {
	select {
	case <-ctx.Done():
		return OnceOutcomeCanceled
	case <-g.done:
		return OnceOutcomeJobDone
	case <-g.started:
		// claim 済み。下の waitDone へ落ちる。
	case <-timeout:
		// **タイマーと claim が同時に成立した場合は claim を優先する。**
		// select は準備できた case をランダムに選ぶので、ここで started を
		// 見直さないと、実行中のジョブを抱えたまま idle_timeout を返す ---
		// 呼び出し側はそれを見てプロセスを畳むので、**実行中のジョブが
		// drain のタイムアウト（30 秒）で打ち切られうる**。数時間の
		// エンコードを打ち切らないことが ScaledJob を選んだ理由そのもの
		// （docs/operations.md §5）。
		select {
		case <-g.started:
			// claim 済み。下の waitDone へ落ちる。
		default:
			return OnceOutcomeIdleTimeout
		}
	}

	return g.waitDone(ctx)
}

// waitDone は claim 済みのジョブが Work を抜けるまで待つ（タイムアウトなし）。
func (g *OnceGate) waitDone(ctx context.Context) OnceOutcome {
	select {
	case <-ctx.Done():
		return OnceOutcomeCanceled
	case <-g.done:
		return OnceOutcomeJobDone
	}
}
