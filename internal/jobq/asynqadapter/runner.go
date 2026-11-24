package asynqadapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/hibiken/asynq"

	"github.com/fetburner/rokuban/internal/jobq"
)

type serverRun struct {
	workCtx        context.Context
	cancelWork     context.CancelFunc
	cancelPeriodic context.CancelFunc
	periodic       sync.WaitGroup
	servers        []*asynq.Server
	done           chan struct{}
	drained        chan struct{}
	once           *onceGate

	// Adapter.mu guards admission to Work and the count of admitted handlers.
	stopping bool
	active   int
}

// Start は通常モードで物理キューごとに新しい Server を起動する。
func (adapter *Adapter) Start(ctx context.Context) error {
	if adapter.once {
		return errors.New("asynqadapter: once mode requires RunOnce")
	}
	_, err := adapter.start(ctx, nil)
	return err
}

func (adapter *Adapter) start(ctx context.Context, gate *onceGate) (*serverRun, error) {
	for {
		adapter.mu.Lock()
		run := adapter.run
		switch {
		case ctx.Err() != nil:
			adapter.mu.Unlock()
			return nil, fmt.Errorf("starting asynq adapter: %w", ctx.Err())
		case run == nil:
			run, err := adapter.startLocked(ctx, gate)
			adapter.mu.Unlock()
			return run, err
		case !run.stopping:
			adapter.mu.Unlock()
			return nil, errors.New("asynqadapter: delivery is already running")
		default:
			adapter.mu.Unlock()
			select {
			case <-run.done:
			case <-ctx.Done():
				return nil, fmt.Errorf("waiting for previous asynq shutdown: %w", ctx.Err())
			}
		}
	}
}

func (adapter *Adapter) startLocked(ctx context.Context, gate *onceGate) (*serverRun, error) {
	base := context.WithoutCancel(ctx)
	workCtx, cancelWork := context.WithCancel(base)
	periodicCtx, cancelPeriodic := context.WithCancel(base)
	run := &serverRun{
		workCtx: workCtx, cancelWork: cancelWork, cancelPeriodic: cancelPeriodic,
		done: make(chan struct{}), drained: make(chan struct{}), once: gate,
	}
	adapter.run = run
	for queue, concurrency := range adapter.queues {
		server := asynq.NewServerFromRedisClient(adapter.redis, asynq.Config{
			Queues:                   map[string]int{queue: 1},
			Concurrency:              concurrency,
			BaseContext:              func() context.Context { return base },
			ShutdownTimeout:          time.Second,
			DelayedTaskCheckInterval: delayedTaskCheckInterval,
			IsFailure:                isFailure,
			RetryDelayFunc:           retryDelay,
			Logger:                   asynqLogger{},
		})
		handler := asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			return adapter.work(ctx, run, server, task)
		})
		if err := server.Start(handler); err != nil {
			adapter.stopLocked(run)
			return nil, fmt.Errorf("starting asynq server for queue %q: %w", queue, err)
		}
		run.servers = append(run.servers, server)
	}
	for _, periodic := range adapter.periodic {
		run.periodic.Go(func() { adapter.enqueuePeriodic(periodicCtx, periodic) })
	}
	return run, nil
}

func (adapter *Adapter) enqueuePeriodic(ctx context.Context, periodic jobq.PeriodicJobSpec) {
	ticker := time.NewTicker(periodic.Interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if _, err := adapter.Enqueue(ctx, periodic.Args); err != nil && ctx.Err() == nil {
			slog.Error("asynq periodic enqueue", "kind", periodic.Args.Kind(), "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop は新しい Work の開始を止め、ctx の期限で実行中の Work を取り消す。
func (adapter *Adapter) Stop(ctx context.Context) error {
	return adapter.stop(ctx, nil)
}

// stop は run を停止する。nil なら現在の run を対象にする。
func (adapter *Adapter) stop(ctx context.Context, run *serverRun) error {
	adapter.mu.Lock()
	if run == nil {
		run = adapter.run
	}
	if run == nil {
		adapter.mu.Unlock()
		return nil
	}
	if !run.stopping {
		adapter.stopLocked(run)
	}
	adapter.mu.Unlock()

	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		run.cancelWork()
		return fmt.Errorf("stopping asynq adapter: %w", ctx.Err())
	}
}

func (adapter *Adapter) stopLocked(run *serverRun) {
	run.stopping = true
	if run.active == 0 {
		close(run.drained)
	}
	run.cancelPeriodic()
	go adapter.shutdown(run)
}

func (adapter *Adapter) shutdown(run *serverRun) {
	var servers sync.WaitGroup
	for _, server := range run.servers {
		servers.Go(func() { run.stopServer(server) })
	}
	servers.Wait()

	// Shutdown の固定猶予は Redis への結果反映に使う。Work 自体には
	// 呼び出しごとの Stop ctx が与える猶予を使う。
	select {
	case <-run.drained:
	case <-run.workCtx.Done():
	}
	for _, server := range run.servers {
		servers.Go(server.Shutdown)
	}
	servers.Wait()
	run.periodic.Wait()
	run.cancelWork()

	adapter.mu.Lock()
	adapter.run = nil
	close(run.done)
	adapter.mu.Unlock()
}

// stopServer はハンドラと shutdown が同じ Server の取得停止を共有できるようにする。
func (run *serverRun) stopServer(server *asynq.Server) {
	if run.once != nil {
		run.once.stopPulling.Do(server.Stop)
		return
	}
	server.Stop()
}

func (adapter *Adapter) work(ctx context.Context, run *serverRun, server *asynq.Server, task *asynq.Task) error {
	adapter.mu.Lock()
	if run.stopping {
		adapter.mu.Unlock()
		return jobq.Snooze(0)
	}
	run.active++
	adapter.mu.Unlock()
	defer func() {
		adapter.mu.Lock()
		run.active--
		if run.stopping && run.active == 0 {
			close(run.drained)
		}
		adapter.mu.Unlock()
	}()

	outcome := jobq.OnceOutcomeJobUnhandled
	if gate := run.once; gate != nil {
		if !gate.admissionClosed.CompareAndSwap(false, true) {
			return jobq.Snooze(0)
		}
		// 並列数 1 の枠を持つ間に取得を止める（TestRunOnceConsumesOnlyOneTask）。
		run.stopServer(server)
		defer func() { gate.done <- outcome }()
	}
	args, err := adapter.registry.Decode(task.Type(), task.Payload())
	if err != nil {
		return fmt.Errorf("decoding asynq task: %w: %w", err, asynq.RevokeTask)
	}
	spec := args.Spec()
	limit := spec.MaxAttempts
	if limit == 0 {
		limit = DefaultMaxAttempts
	}
	retries, _ := asynq.GetRetryCount(ctx)
	// プロセス死の失敗は asynq が回収時に数える。上限に達したタスクは
	// Redis から取り除くために受け取るが、Work には渡さない。
	if retries >= limit {
		return fmt.Errorf("asynq job %q exhausted attempts during recovery: %w", args.Kind(), asynq.RevokeTask)
	}

	workCtx, cancel := context.WithTimeout(ctx, spec.Timeout(args))
	defer cancel()
	stop := context.AfterFunc(run.workCtx, cancel)
	defer stop()
	if run.workCtx.Err() != nil {
		return jobq.Snooze(0)
	}
	outcome = jobq.OnceOutcomeJobDone
	err = adapter.registry.Work(workCtx, args)
	if ctxErr := workCtx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.Canceled) && run.workCtx.Err() != nil {
			return jobq.Snooze(0)
		}
		err = ctxErr
	}
	if err == nil {
		return nil
	}
	if _, snoozed := jobq.SnoozeDelay(err); snoozed {
		return err
	}
	if errors.Is(err, jobq.ErrDiscard) || retries >= limit-1 {
		return fmt.Errorf("asynq job %q: %w: %w", args.Kind(), err, asynq.RevokeTask)
	}
	return fmt.Errorf("working asynq job %q: %w", args.Kind(), err)
}

func isFailure(err error) bool {
	_, snoozed := jobq.SnoozeDelay(err)
	return !snoozed
}

// retryDelay は Snooze と lease 切れの待ち時間を個別に扱う。
// adapter を差し替えても通常の失敗の再試行間隔が変わらないよう、
// River の既定と同じく、失敗した試行番号の 4 乗秒を基準にする。
func retryDelay(retries int, err error, _ *asynq.Task) time.Duration {
	if delay, snoozed := jobq.SnoozeDelay(err); snoozed {
		// asynq v0.26.0 は再試行時刻を秒に切り捨てる。正の
		// Snooze は期限より早く配らない（TestContract の Snooze delay）。
		if delay > 0 {
			delay += min(time.Second, time.Duration(1<<63-1)-delay)
		}
		return delay
	}
	if errors.Is(err, asynq.ErrLeaseExpired) {
		return 0
	}
	seconds := math.Pow(float64(retries)+1, 4)
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds) * time.Second
}
