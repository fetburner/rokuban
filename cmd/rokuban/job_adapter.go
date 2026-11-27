package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/asynqadapter"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobq/riverbind"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/worker"
)

type workerQueue interface {
	jobq.Enqueuer
	jobq.Runner
	RunOnce(waitCtx, signalCtx context.Context, idleTimeout time.Duration, cancelProcess func()) (jobq.OnceOutcome, error)
}

type workerClientConfig struct {
	Spec                 jobq.WorkerSpec
	SchedulePeriodicJobs bool
	Queues               []string
	Once                 bool
	SoftStopTimeout      time.Duration
}

// newWorkerClientConfig は DB pool に依存せず、worker の製品仕様と起動形態を組み立てる。
// 購読・定期投入の内容は製品の仕様（workerspec.go）が決める。
func newWorkerClientConfig(cfg *config.Config, bound []config.MirakcSite, mounted []string, pruner store.NotificationPruner, queues []string, once bool, softStopTimeout time.Duration) workerClientConfig {
	return workerClientConfig{
		Spec:                 newWorkerSpec(cfg, bound, mounted, pruner),
		SchedulePeriodicJobs: cfg.Worker.PeriodicJobs,
		Queues:               queues,
		Once:                 once,
		SoftStopTimeout:      softStopTimeout,
	}
}

// newInsertOnlyEnqueuer は api / watcher / enqueue CLI の投入と notifier の件数取得に使う adapter を作る。
func newInsertOnlyEnqueuer(ctx context.Context, cfg *config.Config, pool *dbHandle) (jobq.Enqueuer, func() error, error) {
	return newJobAdapter(ctx, cfg, pool, nil, workerClientConfig{})
}

// newJobAdapter は設定に従って adapter を構築する。base が nil なら購読を持たない。
// 投入先は jobs.PhysicalQueueFor で決める。購読側の物理名を展開する newWorkerSpec
// とは別の経路であり、各 adapter は定期ジョブについて両者の一致を検査する。
func newJobAdapter(ctx context.Context, cfg *config.Config, pool *dbHandle, base *worker.Deps, clientCfg workerClientConfig) (workerQueue, func() error, error) {
	if cfg.Worker.Engine == config.JobEngineAsynq {
		registry := jobq.NewRegistry()
		if base != nil {
			base.RecoveryGrace = asynqadapter.RecoveryGrace
			registry = worker.NewWorkers(base)
		}
		adapter, closeAdapter, err := asynqadapter.NewFromRedisURL(cfg.Worker.RedisURL, registry, jobs.PhysicalQueueFor, asynqadapter.Config{
			Spec:                 clientCfg.Spec,
			Queues:               clientCfg.Queues,
			SchedulePeriodicJobs: clientCfg.SchedulePeriodicJobs,
			Once:                 clientCfg.Once,
			SoftStopTimeout:      clientCfg.SoftStopTimeout,
		})
		if err != nil {
			return nil, nil, err
		}
		return adapter, closeAdapter, nil
	}

	if base == nil {
		var closePool func() error
		if pool == nil {
			// 単発 CLI はロールを持たず、pgxpool の既定の上限を使う。
			var err error
			pool, err = openDB(ctx, cfg, nil, 0, 0)
			if err != nil {
				return nil, nil, err
			}
			closePool = func() error {
				pool.Close()
				return nil
			}
		}
		adapter, err := riveradapter.NewInsertOnlyEnqueuer(pool.pgxPool(), jobs.PhysicalQueueFor)
		if err != nil {
			closeJobAdapter(closePool)
			return nil, nil, err
		}
		return adapter, closePool, nil
	}
	riverConfig := riveradapter.Config{
		Spec:                 clientCfg.Spec,
		Queues:               clientCfg.Queues,
		SchedulePeriodicJobs: clientCfg.SchedulePeriodicJobs,
		Once:                 clientCfg.Once,
		SoftStopTimeout:      clientCfg.SoftStopTimeout,
	}
	if cfg.Worker.RescueStuckJobsAfter != nil {
		riverConfig.RescueStuckJobsAfter = *cfg.Worker.RescueStuckJobsAfter
	}
	base.RecoveryGrace = riverConfig.RecoveryGrace()
	registry := worker.NewWorkers(base)
	if err := riverbind.BindAll(registry); err != nil {
		return nil, nil, err
	}
	adapter, err := riveradapter.NewAdapter(pool.pgxPool(), registry, jobs.PhysicalQueueFor, riverConfig)
	if err != nil {
		return nil, nil, err
	}
	return adapter, nil, nil
}

func closeJobAdapter(closeAdapter func() error) {
	if closeAdapter != nil {
		if err := closeAdapter(); err != nil {
			slog.Error("closing job queue connection", "err", err)
		}
	}
}
