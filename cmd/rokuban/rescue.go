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
// （rules / recordings / media_assets / drop_stats / program_intents /
// program_overrides）を DB に冪等 upsert する。catalog が無いファイルからの
// 素の asset 登録は後続（M3-10 と骨格共有）に回す最小骨格。
func newRescueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rescue",
		Short: "catalog からコアメタデータを DB に復元する",
		Long: `media_dir/catalog/ 配下の最新 catalog JSON を読み、ルール・録画・
media_assets・ドロップ統計・手動意図/上書きを Postgres に冪等 upsert する
（docs/storage.md §8、災害復旧）。

catalog が無いメディアファイルの「素の asset」登録は未実装（M3-10 と共有予定）。
再実行しても増殖しない（ON CONFLICT で上書き）。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			pool, err := db.NewPool(ctx, cfg.DB)
			if err != nil {
				return err
			}
			defer pool.Close()

			return runRescue(ctx, pool, cfg.Storage.MediaDir, cmd.OutOrStdout())
		},
	}
	return cmd
}

// runRescue は catalog 復元の本体。cobra の RunE は配線に留め、DB / ファイル
// 操作はここに閉じ込める（runShadowDiff / runEnqueue と同じ切り出し）。
func runRescue(ctx context.Context, pool *pgxpool.Pool, mediaDir string, out io.Writer) error {
	result, err := catalog.RescueLatest(ctx, pool, mediaDir)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "rescued from %s\n", result.CatalogPath)
	_, _ = fmt.Fprintf(out, "  rules:              %d\n", result.Rules)
	_, _ = fmt.Fprintf(out, "  recordings:         %d\n", result.Recordings)
	_, _ = fmt.Fprintf(out, "  media_assets:       %d\n", result.MediaAssets)
	_, _ = fmt.Fprintf(out, "  drop_stats:         %d\n", result.DropStats)
	_, _ = fmt.Fprintf(out, "  program_snapshots:  %d\n", result.ProgramSnapshots)
	_, _ = fmt.Fprintf(out, "  program_intents:    %d\n", result.ProgramIntents)
	_, _ = fmt.Fprintf(out, "  program_overrides:  %d\n", result.ProgramOverrides)
	return nil
}
