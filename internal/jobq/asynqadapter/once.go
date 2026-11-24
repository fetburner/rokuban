package asynqadapter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
)

// delayedTaskCheckInterval は期限が来た再試行・予約タスクを待機列に戻す周期。
// 既存の常駐モードと同じ 1 秒にする。asynq の既定の 5 秒より短く、
// 既定の空振り待ち 30 秒のうちに昇格と取得の機会を確保する。
const delayedTaskCheckInterval = time.Second

// onceGate は最初のハンドラと空振りの確定を直列化する。
type onceGate struct {
	admissionClosed atomic.Bool
	stopPulling     sync.Once
	done            chan jobq.OnceOutcome
}

// RunOnce は 1 件消化用の Server を起動し、終了理由を返す。
// idleTimeout は最初のハンドラに入るまでの待ち時間を制限する。
// waitCtx は終了理由を待つ間に使い、signalCtx は起動と停止に使う。
// SIGTERM などで waitCtx が終了して停止に入ると、実行中の Work の完了を待って canceled を返す（TestRunOnceCanceled）。
// 停止待ちには context.WithoutCancel(signalCtx) を使って --soft-stop-timeout の期限は適用せず、
// k8s では terminationGracePeriodSeconds 経過後の SIGKILL が上限になる（未検証）。
// 終了理由を記録してから Server の停止を待ち、最後に cancelProcess を呼ぶ。
// タスクの失敗は asynq の再試行に委ねる。結果は err が nil のときに有効である。
// 次の RunOnce は新しい Server を作る（TestRunOnceConsumesOnlyOneTask）。
func (adapter *Adapter) RunOnce(waitCtx, signalCtx context.Context, idleTimeout time.Duration, cancelProcess func()) (jobq.OnceOutcome, error) {
	if adapter == nil || !adapter.once {
		return jobq.OnceOutcomeCanceled, errors.New("asynqadapter: once mode is not configured")
	}
	gate := &onceGate{done: make(chan jobq.OnceOutcome, 1)}
	run, err := adapter.start(signalCtx, gate)
	if err != nil {
		return jobq.OnceOutcomeCanceled, err
	}

	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	outcome := gate.wait(waitCtx, timer.C, run.done)
	slog.Info("worker: once mode finished", "outcome", outcome.String())
	if err := adapter.stop(context.WithoutCancel(signalCtx), run); err != nil {
		slog.Error("stopping asynq server (once mode)", "err", err)
	}
	cancelProcess()
	return outcome, nil
}

func (gate *onceGate) wait(ctx context.Context, timeout <-chan time.Time, stopped <-chan struct{}) jobq.OnceOutcome {
	select {
	case outcome := <-gate.done:
		return outcome
	case <-ctx.Done():
		gate.admissionClosed.CompareAndSwap(false, true)
		return jobq.OnceOutcomeCanceled
	case <-stopped:
		return jobq.OnceOutcomeCanceled
	case <-timeout:
		if gate.admissionClosed.CompareAndSwap(false, true) {
			return jobq.OnceOutcomeIdleTimeout
		}
	}

	// ハンドラが先に枠を取った場合は、空振りの期限を外して完了を待つ。
	select {
	case outcome := <-gate.done:
		return outcome
	case <-ctx.Done():
		return jobq.OnceOutcomeCanceled
	case <-stopped:
		return jobq.OnceOutcomeCanceled
	}
}
