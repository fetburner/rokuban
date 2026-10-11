package worker

import (
	"time"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobq"
)

func jobTimeout[T jobq.Args](job *river.Job[T]) time.Duration {
	var args T
	if job != nil {
		args = job.Args
	}
	spec := args.Spec()
	if spec.Timeout == nil {
		return 0
	}
	return spec.Timeout(args)
}
