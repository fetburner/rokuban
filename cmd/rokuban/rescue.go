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
// program_overrides）を DB に冪等 upsert する。catalog が無ければ storage を走査し、
// 認識できる動画ファイルを素の asset として in-place 登録する。
func newRescueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rescue",
		Short: "catalog からコアメタデータを DB に復元する",
		Long: `media_dir/catalog/ 配下の最新 catalog JSON を読み、ルール・録画・
media_assets・ドロップ統計・手動意図/上書きを Postgres に冪等 upsert する
（docs/storage.md §8、災害復旧）。

catalog が無ければ media_dir を走査し、TS / M2TS / MP4 / MKV / WebM を
既存位置のまま素の asset として登録する。再実行しても増殖しない。`,
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
	result, err := catalog.RescueLatest(ctx, pool, mediaDir, db.DefaultSite)
	if err != nil {
		return err
	}

	if result.CatalogPath == "" {
		_, _ = fmt.Fprintln(out, "rescued by scanning media_dir (catalog not found)")
	} else {
		_, _ = fmt.Fprintf(out, "rescued from %s\n", result.CatalogPath)
	}
	_, _ = fmt.Fprintf(out, "  rules:              %d\n", result.Rules)
	_, _ = fmt.Fprintf(out, "  recordings:         %d\n", result.Recordings)
	_, _ = fmt.Fprintf(out, "  media_assets:       %d\n", result.MediaAssets)
	_, _ = fmt.Fprintf(out, "  drop_stats:         %d\n", result.DropStats)
	_, _ = fmt.Fprintf(out, "  program_snapshots:  %d\n", result.ProgramSnapshots)
	_, _ = fmt.Fprintf(out, "  program_intents:    %d\n", result.ProgramIntents)
	_, _ = fmt.Fprintf(out, "  program_overrides:  %d\n", result.ProgramOverrides)
	return nil
}
