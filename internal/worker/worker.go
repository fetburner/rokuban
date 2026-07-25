package worker

import (
	"fmt"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/fetburner/rokuban/internal/mirakc"
)

const (
	defaultIngestConcurrency = 2
	defaultEpgSyncInterval   = 10 * time.Minute
)

// Deps は各ワーカーに注入する依存。
type Deps struct {
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	MediaDir     string

	// EpgRetentionGrace は放送済み番組を刈り取るまでの猶予。0 なら既定値。
	EpgRetentionGrace time.Duration
}

// NewWorkers は全ワーカーを登録した river.Workers を返す。
func NewWorkers(deps *Deps) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &NoOpWorker{})
	river.AddWorker(workers, &IngestWorker{
		MirakcClient: deps.MirakcClient,
		Pool:         deps.Pool,
		MediaDir:     deps.MediaDir,
	})
	river.AddWorker(workers, &EpgSyncWorker{
		MirakcClient:   deps.MirakcClient,
		Pool:           deps.Pool,
		RetentionGrace: deps.EpgRetentionGrace,
	})
	return workers
}

// ClientConfig は River クライアントの設定。
type ClientConfig struct {
	// IngestConcurrency は ingest キューの同時実行数（サイト単位のキャップ）。0 なら既定値。
	IngestConcurrency int

	// EpgSyncSite が空でない場合、そのサイトの EPG 全量同期を定期ジョブとして登録する。
	EpgSyncSite string

	// EpgSyncInterval は EPG 全量同期の間隔。0 なら既定値。
	EpgSyncInterval time.Duration
}

// NewClient は River クライアントを生成する。
func NewClient(pool *pgxpool.Pool, workers *river.Workers, cfg ClientConfig) (*river.Client[pgx5.Tx], error) {
	ingestConcurrency := cfg.IngestConcurrency
	if ingestConcurrency <= 0 {
		ingestConcurrency = defaultIngestConcurrency
	}

	riverCfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 100},
			ingestQueue:        {MaxWorkers: ingestConcurrency},
			// 全量同期が重ならないよう 1 本に絞る。
			epgQueue: {MaxWorkers: 1},
		},
		Workers: workers,
	}

	if cfg.EpgSyncSite != "" {
		interval := cfg.EpgSyncInterval
		if interval <= 0 {
			interval = defaultEpgSyncInterval
		}
		site := cfg.EpgSyncSite
		riverCfg.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return EpgSyncArgs{Site: site}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		}
	}

	client, err := river.NewClient(riverpgxv5.New(pool), riverCfg)
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}
	return client, nil
}
