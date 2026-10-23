package jobs

import (
	"context"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobq"
)

// EnqueueContinuation は reconcile の継続ジョブを投入し、結果をログに出す。
// ログの接頭辞は args.Kind() から作る。cursorAttrs は継続が再開する位置、windowAttrs は
// 窓が満杯になった理由で、成功時は windowAttrs、cursorAttrs の順に出す。
// エラーは返さない。投入の失敗でいまのパスを失敗させないためである。
// 失敗で Error、成功で Warn を出すことは TestEnqueueContinuation_LogsFailureAndSuccess が見ている。
// enqueuer は nil でないこと（呼び出し元の Work が先頭で検査する）。
func EnqueueContinuation(ctx context.Context, enqueuer jobq.Enqueuer, args jobq.Args, successMessage string, cursorAttrs, windowAttrs []any) {
	kind := args.Kind()
	if _, err := enqueuer.Enqueue(ctx, args); err != nil {
		attrs := append(append([]any{}, cursorAttrs...), "err", err)
		slog.Error(kind+": failed to enqueue continuation", attrs...)
		return
	}
	attrs := append(append([]any{}, windowAttrs...), cursorAttrs...)
	slog.Warn(kind+": "+successMessage, attrs...)
}
