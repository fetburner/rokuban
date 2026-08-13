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

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/worker"
)

// enqueueJob は `rokuban enqueue <name>` 1 件の定義。
//
// RequiresSite が CLI 上の site 束縛 / site 非依存の**唯一の分類**である
// （issue #200）。worker.siteBoundQueueNames からは導出しない --- キュー修飾の
// 集合と「Args に Site が要るか」は一致しない。ruler-pass は mirakc 非依存で
// キューも修飾しないが RulerPassArgs.Site でサイト単位に回すので RequiresSite
// は true。catalog-export はアーカイブが単一なので Site を持たず false。
// 次にジョブを足すときはここに RequiresSite を書いてから factory を足す。
type enqueueJob struct {
	// RequiresSite が true なら --site 解決（多サイトでは必須）を行い、
	// Args に site を渡す。false なら --site を受け付けず（渡されたらエラー）、
	// site 無しで Args を組み立てる。
	RequiresSite bool
	NewArgs      func(site string) river.JobArgs
}

// enqueueJobs はユーザー向けジョブ名（ハイフン区切り）から River の JobArgs を
// 組み立てる索引。ジョブの Kind()（"epg_sync" / "ruler_pass" 等）とは別に、
// CLI では読みやすい名前を使う。
//
// delete_reconcile はここに載せない。PeriodicJobs（または worker 側の定期）が
// 投入するだけで、手動 enqueue の対象にしていない（catalog-export だけが
// cleanup 系で enqueue 可能。issue #200 の洗い出し）。
var enqueueJobs = map[string]enqueueJob{
	"epg-sync": {
		RequiresSite: true,
		NewArgs:      func(site string) river.JobArgs { return worker.EpgSyncArgs{Site: site} },
	},
	"tuner-sync": {
		RequiresSite: true,
		NewArgs:      func(site string) river.JobArgs { return worker.TunerSyncArgs{Site: site} },
	},
	"ruler-pass": {
		// キューは site 非修飾（siteBoundQueueNames 外）だが Args.Site 必須。
		RequiresSite: true,
		NewArgs:      func(site string) river.JobArgs { return worker.RulerPassArgs{Site: site} },
	},
	"reconcile-pass": {
		RequiresSite: true,
		NewArgs:      func(site string) river.JobArgs { return worker.ReconcilePassArgs{Site: site} },
	},
	"record-sweep": {
		RequiresSite: true,
		NewArgs:      func(site string) river.JobArgs { return worker.RecordSweepArgs{Site: site} },
	},
	"catalog-export": {
		RequiresSite: false,
		NewArgs:      func(string) river.JobArgs { return worker.CatalogExportArgs{} },
	},
	"encode-reconcile": {
		// エンコードは site の属性を持たない（アーカイブもプロファイルも単一。
		// worker.EncodeReconcileArgs のコメント）。delete_reconcile と違って
		// 何も削除せず、DB を読んで encode ジョブを投入するだけなので、
		// `worker.periodic_jobs: false` の構成（k8s）で CronJob から叩けるよう
		// ここに載せる --- 載せないと、その構成ではこの定期パスが一度も走らず
		// issue #163 の穴（ヒントを落とすと黙って再投入されない）が塞がらない。
		RequiresSite: false,
		NewArgs:      func(string) river.JobArgs { return worker.EncodeReconcileArgs{} },
	},
	"storage-sync": {
		// catalog-export と同じ理由（アーカイブ/スクラッチが単一で Site を
		// 持たない。issue #238 M7-5）。delete_reconcile とは異なり読み取り専用
		// （statfs のみ）の観測なので、手動 enqueue の対象から外す理由がない。
		RequiresSite: false,
		NewArgs:      func(string) river.JobArgs { return worker.StorageSyncArgs{} },
	},
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

site 束縛ジョブ（--site が要る）: %s
site 非依存ジョブ（--site を付けない）: %s

UniqueOpts により、同じジョブが既に待機中（available/pending/retryable/running/
scheduled）の場合は新規に投入されず合流する。その場合も終了コード 0 を返す
（CronJob が失敗扱いにならないようにするため）。`,
			strings.Join(sortedJobNamesBySite(true), ", "),
			strings.Join(sortedJobNamesBySite(false), ", ")),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}

			job := args[0]
			site, err := resolveEnqueueJobSite(cmd, job, cfg.Registry())
			if err != nil {
				return err
			}
			// site 束縛ジョブだけ、修飾後キュー名が River 上限を超えないか検証する
			// （site 非依存はキューを修飾しないので検査対象外。issue #185 の「罠」）。
			if site != "" {
				if err := worker.ValidateSiteForQueueNames(site); err != nil {
					return err
				}
			}

			ctx := cmd.Context()
			// 単発 CLI コマンドは特定のロールを担わないので roles は渡さない
			// （pgxpool の既定の MaxConns がそのまま使われる。issue #90）。
			pool, err := db.NewPool(ctx, cfg.DB, nil)
			if err != nil {
				return err
			}
			defer pool.Close()

			return runEnqueue(ctx, pool, job, site, cmd.OutOrStdout())
		},
	}

	cmd.Flags().String("site", "", "対象サイト名（site 束縛ジョブのみ。省略時: レジストリが 1 要素ならその 1 つ、2 要素以上なら必須）")

	return cmd
}

// resolveEnqueueJobSite はジョブ種別と `--site` フラグから、JobArgs に渡す site を決める。
//
// site 非依存ジョブ（enqueueJob.RequiresSite == false）:
//   - `--site` が指定されていればエラー（黙って無視しない。不変条件 10 の精神 /
//     issue #200）。Changed で判定するので `--site=` も拒否する
//   - 未指定なら空文字列を返す（Args は site を使わない）
//
// site 束縛ジョブ:
//   - resolveEnqueueSite と同じ規則（未指定かつレジストリ 1 要素ならその 1 つ、
//     2 要素以上なら必須）
func resolveEnqueueJobSite(cmd *cobra.Command, job string, registry []config.MirakcSite) (string, error) {
	spec, ok := enqueueJobs[job]
	if !ok {
		return "", fmt.Errorf("unknown job: %q (valid: %s)", job, strings.Join(sortedJobNames(), ", "))
	}

	if !spec.RequiresSite {
		if cmd.Flags().Changed("site") {
			return "", fmt.Errorf("job %q is site-independent and does not take --site", job)
		}
		return "", nil
	}

	return resolveEnqueueSite(cmd, registry)
}

// runEnqueue はジョブ名から JobArgs を組み立てて insert-only クライアントで 1 件
// 投入する。DB / River とのやりとりをここに閉じ込め、cobra の RunE は薄い配線に
// とどめる（runShadowDiff と同じ切り出し方）。
//
// site は resolveEnqueueJobSite の結果。site 束縛ジョブでは非空、site 非依存では空。
//
// UniqueOpts による合流で投入されなかった場合も nil を返す（CronJob が失敗扱いに
// ならないようにするため）。投入されたかどうかは out へのログで分かる。
func runEnqueue(ctx context.Context, pool *pgxpool.Pool, job, site string, out io.Writer) error {
	spec, ok := enqueueJobs[job]
	if !ok {
		return fmt.Errorf("unknown job: %q (valid: %s)", job, strings.Join(sortedJobNames(), ", "))
	}

	client, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		return err
	}

	res, err := client.Insert(ctx, spec.NewArgs(site), nil)
	if err != nil {
		return fmt.Errorf("inserting job %q: %w", job, err)
	}

	suffix := ""
	if site != "" {
		suffix = fmt.Sprintf(" for site %q", site)
	}
	if res.UniqueSkippedAsDuplicate {
		_, _ = fmt.Fprintf(out, "job %q already pending%s, not inserted\n", job, suffix)
		return nil
	}
	_, _ = fmt.Fprintf(out, "inserted job %q (id=%d)%s\n", job, res.Job.ID, suffix)
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

// sortedJobNamesBySite は RequiresSite が want と一致するジョブ名をソートして返す
// （help 文言とテスト用）。
func sortedJobNamesBySite(want bool) []string {
	names := make([]string, 0, len(enqueueJobs))
	for name, spec := range enqueueJobs {
		if spec.RequiresSite == want {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
