package worker

import (
	"context"

	"github.com/riverqueue/river"
)

type NoOpArgs struct{}

func (NoOpArgs) Kind() string { return "noop" }

type NoOpWorker struct {
	river.WorkerDefaults[NoOpArgs]
}

func (w *NoOpWorker) Work(ctx context.Context, job *river.Job[NoOpArgs]) error {
	return nil
}
