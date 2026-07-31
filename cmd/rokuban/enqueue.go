package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/worker"
)

// enqueueJobs はユーザー向けジョブ名（ハイフン区切り）から River の JobArgs を組み立てる
// 関数の索引。ジョブの Kind()（"epg_sync" / "ruler_pass" / "reconcile_pass"）とは別に、
// CLI では読みやすい名前を使う。
var enqueueJobs = map[string]func(site string) river.JobArgs{
	"epg-sync":       func(site string) river.JobArgs { return worker.EpgSyncArgs{Site: site} },
	"tuner-sync":     func(site string) river.JobArgs { return worker.TunerSyncArgs{Site: site} },
	"ruler-pass":     func(site string) river.JobArgs { return worker.RulerPassArgs{Site: site} },
	"reconcile-pass": func(site string) river.JobArgs { return worker.ReconcilePassArgs{Site: site} },
	"record-sweep":   func(site string) river.JobArgs { return worker.RecordSweepArgs{Site: site} },
	// catalog-export はサイト非依存（site 引数は無視する）。
	"catalog-export": func(string) river.JobArgs { return worker.CatalogExportArgs{} },
}

// newEnqueueCmd は `rokuban enqueue <job>` サブコマンドを作る。
//
// worker.periodic_jobs を無効化したデプロイ（k8s）では、CronJob がこのコマンドを
// 叩いて定期ジョブを投入する（docs/data.md §2「定期実行の契機はデプロイ形態に委ねる」）。
// 手動での即時実行にも使える。insert-only の River クライアントで 1 件投入して即終了する
// ため、worker のワーカー群（ingest/encode 等）はこのプロセスに一切紐付かない。
func newEnqueueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enqueue <job>",
		Short: "定期ジョブを 1 件投入する",
		Long: fmt.Sprintf(`定期ジョブを 1 件投入して即終了する（k8s CronJob 用途 / 手動即時実行用）。

対応ジョブ: %s

UniqueOpts により、同じジョブが既に待機中（available/pending/retryable/running/
scheduled）の場合は新規に投入されず合流する。その場合も終了コード 0 を返す
（CronJob が失敗扱いにならないようにするため）。`, strings.Join(sortedJobNames(), ", ")),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			// 単発 CLI コマンドは特定のロールを担わないので roles は渡さない
			// （pgxpool の既定の MaxConns がそのまま使われる。issue #90）。
			pool, err := db.NewPool(ctx, cfg.DB, nil)
			if err != nil {
				return err
			}
			defer pool.Close()

			return runEnqueue(ctx, pool, args[0], cfg.Mirakc.Site, cmd.OutOrStdout())
		},
	}

	return cmd
}

// runEnqueue はジョブ名から JobArgs を組み立てて insert-only クライアントで 1 件
// 投入する。DB / River とのやりとりをここに閉じ込め、cobra の RunE は薄い配線に
// とどめる（runShadowDiff と同じ切り出し方）。
//
// サイトは config.mirakc.site（issue #31）から渡される。
//
// UniqueOpts による合流で投入されなかった場合も nil を返す（CronJob が失敗扱いに
// ならないようにするため）。投入されたかどうかは out へのログで分かる。
func runEnqueue(ctx context.Context, pool *pgxpool.Pool, job, site string, out io.Writer) error {
	newArgs, ok := enqueueJobs[job]
	if !ok {
		return fmt.Errorf("unknown job: %q (valid: %s)", job, strings.Join(sortedJobNames(), ", "))
	}

	client, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		return err
	}

	res, err := client.Insert(ctx, newArgs(site), nil)
	if err != nil {
		return fmt.Errorf("inserting job %q: %w", job, err)
	}

	if res.UniqueSkippedAsDuplicate {
		_, _ = fmt.Fprintf(out, "job %q already pending for site %q, not inserted\n", job, site)
		return nil
	}
	_, _ = fmt.Fprintf(out, "inserted job %q (id=%d) for site %q\n", job, res.Job.ID, site)
	return nil
}

func sortedJobNames() []string {
	names := make([]string, 0, len(enqueueJobs))
	for name := range enqueueJobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
