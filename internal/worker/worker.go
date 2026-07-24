package worker

import (
	"fmt"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/fetburner/rokuban/internal/mirakc"
)

type IngestDeps struct {
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	MediaDir     string
}

func NewWorkers(deps *IngestDeps) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &NoOpWorker{})
	river.AddWorker(workers, &IngestWorker{
		MirakcClient: deps.MirakcClient,
		Pool:         deps.Pool,
		MediaDir:     deps.MediaDir,
	})
	return workers
}

func NewClient(pool *pgxpool.Pool, workers *river.Workers, ingestConcurrency int) (*river.Client[pgx5.Tx], error) {
	if ingestConcurrency <= 0 {
		ingestConcurrency = 2
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 100},
			ingestQueue:        {MaxWorkers: ingestConcurrency},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}
	return client, nil
}
