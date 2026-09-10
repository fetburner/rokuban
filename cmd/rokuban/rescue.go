package main

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/db"
)

// newRescueCmd は `rokuban rescue` サブコマンドを作る（M3-9 / issue #71）。
//
// media_dir/catalog/ の最新 catalog JSON を読み、コアメタデータ
// （rules / recordings / media_assets / drop_stats / drop_positions / program_intents /
// program_overrides）を DB に冪等 upsert する。catalog が無ければ storage を走査し、
// `sites/{site}/` 前置を持つ認識可能な動画ファイルを素の asset として in-place 登録する。
func newRescueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rescue",
		Short: "catalog からコアメタデータを DB に復元する",
		Long: `media_dir/catalog/ 配下の最新 catalog JSON を読み、ルール・録画・
media_assets・ドロップ統計・手動意図/上書きを Postgres に冪等 upsert する
（docs/storage.md §8、災害復旧）。

catalog が無ければ media_dir を走査し、sites/{site}/ 前置を持つ TS / M2TS /
MP4 / MKV / WebM を既存位置のまま素の asset として登録する。前置の無いファイルは
登録しない。再実行しても増殖しない。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			// 単発 CLI コマンドは特定のロールを担わないので roles は渡さない
			// （pgxpool の既定の MaxConns がそのまま使われる。issue #90）。
			pool, err := db.NewPool(ctx, cfg.DB, nil, 0)
			if err != nil {
				return err
			}
			defer pool.Close()

			return runRescue(ctx, pool, cfg.Storage.MediaDir, registryNames(cfg.Registry()), cmd.OutOrStdout())
		},
	}
	return cmd
}

// runRescue は catalog 復元の本体。cobra の RunE は配線に留め、DB / ファイル
// 操作はここに閉じ込める（runShadowDiff / runEnqueue と同じ切り出し）。
//
// registrySites は `mirakcs:` レジストリの site 名一覧
// （catalog.RescueLatest 参照。ストレージ走査で見つけた sites/{site}/ 前置の
// site がタイポかどうかの判定に使う）。
func runRescue(ctx context.Context, pool *pgxpool.Pool, mediaDir string, registrySites []string, out io.Writer) error {
	result, err := catalog.RescueLatest(ctx, pool, mediaDir, registrySites)
	if err != nil {
		return err
	}

	if result.CatalogPath == "" {
		// 「catalog が 1 つも無い」と「あったが全部不完全だった」を区別して出す。
		if len(result.RejectedSnapshots) > 0 {
			_, _ = fmt.Fprintln(out, "rescued by scanning media_dir (no complete catalog generation)")
		} else {
			_, _ = fmt.Fprintln(out, "rescued by scanning media_dir (catalog not found)")
		}
	} else {
		_, _ = fmt.Fprintf(out, "rescued from %s\n", result.CatalogPath)
	}
	// 「最新に見えたものを飛ばした」ことは黙って成功させない（docs/storage.md §8）。
	for _, r := range result.RejectedSnapshots {
		_, _ = fmt.Fprintf(out, "  skipped incomplete generation %s: %s\n", r.Name, r.Reason)
	}
	_, _ = fmt.Fprintf(out, "  rules:              %d\n", result.Rules)
	_, _ = fmt.Fprintf(out, "  recordings:         %d\n", result.Recordings)
	_, _ = fmt.Fprintf(out, "  media_assets:       %d\n", result.MediaAssets)
	_, _ = fmt.Fprintf(out, "  drop_stats:         %d\n", result.DropStats)
	_, _ = fmt.Fprintf(out, "  drop_positions:     %d\n", result.DropPositions)
	_, _ = fmt.Fprintf(out, "  program_snapshots:  %d\n", result.ProgramSnapshots)
	_, _ = fmt.Fprintf(out, "  program_intents:    %d\n", result.ProgramIntents)
	_, _ = fmt.Fprintf(out, "  program_overrides:  %d\n", result.ProgramOverrides)
	// 落とした行は黙って切り捨てない。永続資産は復元できているので rescue 自体は
	// 成功だが、ダンプが壊れている事実は運用者に伝える。
	if result.SkippedProgramSnapshots > 0 {
		_, _ = fmt.Fprintf(out,
			"  warning: skipped %d program_snapshots the database would reject "+
				"(and %d program_intents / %d program_overrides that referenced them)\n",
			result.SkippedProgramSnapshots, result.SkippedProgramIntents, result.SkippedProgramOverrides)
	}
	return nil
}
