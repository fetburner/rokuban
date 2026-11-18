package jobs

import (
	"context"
	"errors"

	"github.com/fetburner/rokuban/internal/jobq"
)

// Enqueue は後続ジョブ args を enqueuer へ投入する。投入はデータ更新の commit 後のヒントで
// あり、失敗しても定期パスが拾い直すため、呼び出し側は戻り値をログに残すだけでよい
// （不変条件 5）。enqueuer が nil なら投入せずエラーを返す。
//
// jobs に置くのは、呼び出し元（internal/worker と各ドメインパッケージ）が同じ理由で
// 変わるからであり、VerifySite と同じ判断による。
func Enqueue(ctx context.Context, enqueuer jobq.Enqueuer, args jobq.Args) error {
	if enqueuer == nil {
		return errors.New("jobs: job enqueuer is nil")
	}
	_, err := enqueuer.Enqueue(ctx, args)
	return err
}
