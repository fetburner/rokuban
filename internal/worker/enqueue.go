package worker

import (
	"context"
	"errors"

	"github.com/fetburner/rokuban/internal/jobq"
)

func enqueueJob(ctx context.Context, enqueuer jobq.Enqueuer, args jobq.Args) error {
	if enqueuer == nil {
		return errors.New("worker: job enqueuer is nil")
	}
	_, err := enqueuer.Enqueue(ctx, args)
	return err
}
