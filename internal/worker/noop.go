package worker

import (
	"context"

	"github.com/riverqueue/river"
)

// NoOpArgs は no-op ジョブの引数。River の動作確認用。
type NoOpArgs struct{}

// Kind は River ジョブの種別名を返す。
func (NoOpArgs) Kind() string { return "noop" }

// NoOpWorker は何もしない River ワーカー。
type NoOpWorker struct {
	river.WorkerDefaults[NoOpArgs]
}

// Work は何もせずに成功を返す。
func (w *NoOpWorker) Work(ctx context.Context, job *river.Job[NoOpArgs]) error {
	return nil
}
