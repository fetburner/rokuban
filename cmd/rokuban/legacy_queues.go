package main

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
)

// legacyUnqualifiedQueues は、置き場で修飾する前は修飾の無い名前で使っていたキュー。
// いまは `{queue}_{store}` にだけ積まれ、修飾の無い名前のキューを購読する worker は無い。
// 置き場の導入前に積まれたジョブは、誰にも掴まれないまま残る。
// TestLegacyQueuesAreStoreBoundToday が、この一覧が置き場で修飾する論理キューであることを固定する。
var legacyUnqualifiedQueues = []string{
	jobs.EncodeQueue, jobs.ThumbnailQueue, jobs.CMDetectQueue, jobs.TSScanQueue, jobs.StorageQueue,
}

// legacyCleanupKinds は、以前 cleanup キューに載り、いまは置き場の original キューに載るジョブ種別。
// cleanup キューは今も購読されるので、置き場の無い古いジョブは worker に届いて置き場の照合で失敗し続ける。
var legacyCleanupKinds = []string{
	jobs.DeleteReconcileArgs{}.Kind(), jobs.CatalogExportArgs{}.Kind(),
}

// legacyQueueSelectors は取り消す対象を返す。
func legacyQueueSelectors() []riveradapter.QueuedJobSelector {
	selectors := make([]riveradapter.QueuedJobSelector, 0, len(legacyUnqualifiedQueues)+1)
	for _, queue := range legacyUnqualifiedQueues {
		selectors = append(selectors, riveradapter.QueuedJobSelector{Queue: queue})
	}
	return append(selectors, riveradapter.QueuedJobSelector{Queue: jobs.CleanupQueue, Kinds: legacyCleanupKinds})
}

// newQueuesCmd は `rokuban queues` を作る。
func newQueuesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "queues",
		Short: "ジョブキューを管理する",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "cancel-legacy",
		Short: "置き場で修飾する前のキューに残ったジョブを取り消す",
		Long: `置き場で修飾する前の、修飾の無い名前のキュー（encode / thumbnail / cm_detect / ts_scan / storage）に
残った未完了のジョブと、cleanup キューに残った delete_reconcile / catalog_export を取り消す。

置き場で修飾するキューの導入後は、それらのジョブを掴む worker が無い。取り消した仕事は、レベルトリガーの
reconcile（定期パス）が、原本のコピーのある置き場のキューへ積み直す。何度実行しても安全で、完了済みのジョブには触れない。
River の公開 API（JobList / JobCancel）だけを使う。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			pool, err := db.NewPool(cmd.Context(), cfg.DB, nil, 0, 0)
			if err != nil {
				return err
			}
			defer pool.Close()
			return runCancelLegacyQueues(cmd.Context(), pool, cmd.OutOrStdout())
		},
	})
	return cmd
}

// runCancelLegacyQueues は旧キューの未完了のジョブを取り消し、件数を out に書く。
func runCancelLegacyQueues(ctx context.Context, pool *pgxpool.Pool, out io.Writer) error {
	adapter, err := newInsertOnlyEnqueuer(pool)
	if err != nil {
		return err
	}
	cancelled, err := adapter.CancelQueued(ctx, legacyQueueSelectors())
	if err != nil {
		return fmt.Errorf("cancelling legacy queue jobs: %w", err)
	}
	_, _ = fmt.Fprintf(out, "cancelled %d job(s) in the legacy unqualified queues\n", cancelled)
	return nil
}
