package worker

import (
	"context"
	"errors"

	"github.com/fetburner/rokuban/internal/jobq"
)

// enqueueJob は args を enqueuer に投入する。enqueuer が nil なら投入せずエラーを返す。
// worker の外に置いたジョブ worker は自分で nil を確かめて Enqueue を直に呼ぶので、
// この関数は worker に置く。
func enqueueJob(ctx context.Context, enqueuer jobq.Enqueuer, args jobq.Args) error {
	if enqueuer == nil {
		return errors.New("worker: job enqueuer is nil")
	}
	_, err := enqueuer.Enqueue(ctx, args)
	return err
}
