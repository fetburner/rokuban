package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

const (
	// recordingsNotifyTopic は録画の一覧・棚を購読する SSE クライアントへ配る
	// トピック名（recordings_notify / media_assets_notify トリガーと同じ）。
	recordingsNotifyTopic = "recordings"

	// defaultLabelRuleReconcileInterval は定期の全件再評価の既定間隔。
	//
	// これは必須の窓閉じである。分類ルールの編集の前後でコミットした録画は、
	// 旧ルールで評価された当たりを持ったままになる（トリガーは自分の行しか
	// 見ない）。encode 系の定期パスと同じ 15 分に揃える。
	defaultLabelRuleReconcileInterval = 15 * time.Minute
)

// LabelRuleReconcileWorker は分類ルールの当たりを録画全件へ適用し直す。
//
// 起動の契機は 3 つあり、どれもこの 1 つのワーカーに集まる。
//
//   - /api/label-rules の変更（作成・keyword/priority 変更・削除）が同じ tx で
//     投入する
//   - 定期パス（ルールの編集をまたいでコミットした録画の窓を閉じる）
//   - `rokuban enqueue label-rule-reconcile`（k8s はこの経路だけ）
//
// 母集団の条件は入れない。ごみ箱や tombstone の録画もハブの起点になるので、
// 絞るのは棚と一覧の側である（docs/data/series.md §8）。
//
// 評価そのものは SQL 側（label_rule_winner を呼ぶ 1 文）にあり、Go は
// トランザクションと通知だけを持つ。Go 側で評価を書くと、トリガーとジョブの
// 2 人の書き手が別々の規則を持ちうる。
type LabelRuleReconcileWorker struct {
	river.WorkerDefaults[jobs.LabelRuleReconcileArgs]
	Pool *pgxpool.Pool
}

// Work は 1 回の全件再評価を 1 トランザクションで実行する。
//
// **1 トランザクションにするのは、読者に中途半端な棚を見せないためである。**
// 差分適用の途中でコミットすると、一部の録画だけが新しい棚に入った状態が
// 一瞬見える。
//
// 変化があったときだけ recordings トピックへ 1 回通知する。当たりの表に
// 行トリガーを付けると、全件再評価で行数ぶんの通知が出る。
func (w *LabelRuleReconcileWorker) Work(ctx context.Context, _ *river.Job[jobs.LabelRuleReconcileArgs]) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning label rule re-evaluation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 直列化する。並行した 2 本が同じ録画の当たりを同時に更新すると、
	// 古いルール集合で評価した方が後から勝ちうる。
	q := sqlcgen.New(tx)
	if err := q.LockLabelRuleReevaluation(ctx); err != nil {
		return fmt.Errorf("acquiring label rule re-evaluation advisory lock: %w", err)
	}
	changed, err := q.ApplyLabelRuleReevaluation(ctx)
	if err != nil {
		return fmt.Errorf("re-evaluating label rules: %w", err)
	}
	if changed > 0 {
		if err := q.NotifyTopic(ctx, recordingsNotifyTopic); err != nil {
			return fmt.Errorf("notifying recordings after label rule re-evaluation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing label rule re-evaluation: %w", err)
	}
	return nil
}
