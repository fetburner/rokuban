package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/catalog"
)

// newCatalogCmd は `rokuban catalog` サブコマンド群を作る。
func newCatalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "災害復旧用 catalog のユーティリティ",
	}
	cmd.AddCommand(newCatalogVerifyCmd())
	return cmd
}

// newCatalogVerifyCmd は `rokuban catalog verify` を作る。
//
// **DB に一切触らない**（読むのは media_dir/catalog/ だけ）。バックアップの
// 健全性確認に `rokuban rescue` を使うと live DB を catalog の内容で上書きして
// しまうので、確認専用の入口をここに置く（docs/operations.md §3）。
func newCatalogVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "catalog 世代の完成を検証する（DB には触らない）",
		Long: `media_dir/catalog/ の各世代を完成判定に掛けて結果を出す
（manifest の検証 + 構成ファイルのサイズと sha256 の照合。docs/storage.md §8）。

DB には一切触らない。完成世代が 1 つも無ければ非ゼロ終了する。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return runCatalogVerify(cfg.Storage.MediaDir, cmd.OutOrStdout())
		},
	}
}

// runCatalogVerify は catalog 検証の本体。cobra の RunE は配線に留める
// （runRescue と同じ切り出し）。
func runCatalogVerify(mediaDir string, out io.Writer) error {
	statuses, err := catalog.ListSnapshots(mediaDir)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "catalog dir: %s\n", catalog.Dir(mediaDir))

	var complete int
	var selected string
	var newestGeneration *catalog.SnapshotStatus
	for i, st := range statuses {
		if st.Complete {
			_, _ = fmt.Fprintf(out, "  [complete]   %s (schema v%d, exported %s)\n",
				st.Name, st.Manifest.SchemaVersion, st.Manifest.ExportedAt.Format("2006-01-02T15:04:05Z"))
			complete++
			if selected == "" {
				selected = st.Name
			}
		} else {
			_, _ = fmt.Fprintf(out, "  [incomplete] %s: %s\n", st.Name, st.Reason)
		}
		if newestGeneration == nil {
			newestGeneration = &statuses[i]
		}
	}

	if selected == "" {
		return fmt.Errorf("no usable catalog snapshot in %s (rescue would fall back to scanning media_dir)",
			catalog.Dir(mediaDir))
	}
	_, _ = fmt.Fprintf(out, "complete generations: %d\n", complete)
	_, _ = fmt.Fprintf(out, "rescue would use: %s\n", selected)
	// 最新世代が不完全なら、復元は効くが直近のエクスポートは失敗している。
	if newestGeneration != nil && !newestGeneration.Complete {
		_, _ = fmt.Fprintf(out,
			"warning: the newest generation (%s) is incomplete; rescue would fall back to an older snapshot\n",
			newestGeneration.Name)
	}
	return nil
}
