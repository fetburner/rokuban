package worker

import (
	"fmt"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func NewWorkers() *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &NoOpWorker{})
	river.AddWorker(workers, &IngestWorker{})
	return workers
}

func NewClient(pool *pgxpool.Pool, workers *river.Workers) (*river.Client[pgx5.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 100},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}
	return client, nil
}
