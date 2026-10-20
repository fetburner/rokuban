package worker

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// LabelRuleReconcileWorker は分類ルールの当たりを録画全件へ適用し直す。
//
// 起動の契機は 3 つあり、どれもこの worker に集まる。
//
//   - /api/label-rules の変更後に commit してから投入するジョブ
//   - 定期パス（ルール編集をまたいで commit した録画の窓を閉じる）
//   - `rokuban enqueue label-rule-reconcile`（k8s はこの経路だけ）
//
// 永続化と再評価の直列化は store の契約に委ねる。
type LabelRuleReconcileWorker struct {
	Store store.LabelRuleReevaluation
}

// Work は store に全件再評価を委ねる。
func (w *LabelRuleReconcileWorker) Work(ctx context.Context, _ jobs.LabelRuleReconcileArgs) error {
	if _, err := w.Store.ReevaluateAll(ctx); err != nil {
		return fmt.Errorf("re-evaluating label rules: %w", err)
	}
	return nil
}
