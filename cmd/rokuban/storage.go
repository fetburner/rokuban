package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/mediapath"
)

func newStorageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "メディア置き場を管理する",
	}
	cmd.AddCommand(newStorageAdoptCmd())
	return cmd
}

func newStorageAdoptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "adopt <store-name>",
		Short: "media_dir に置き場の目印を書く",
		Args:  cobra.ExactArgs(1),
		Long: `media_dir の root に .rokuban-store を作成し、置き場名を書き込む。
既存ファイルの割り当ては行わない。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			name := args[0]
			if !mediapath.ValidStoreName(name) {
				return fmt.Errorf("invalid store name %q: use lowercase ASCII letters, digits, and hyphens", name)
			}
			if cfg.Storage.MediaStoreName != "" && cfg.Storage.MediaStoreName != name {
				return fmt.Errorf("store name %q does not match storage.media_store_name %q", name, cfg.Storage.MediaStoreName)
			}
			if err := mediapath.WriteStoreMarker(cfg.Storage.MediaDir, name); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s for store %q\n", mediapath.StoreMarkerFileName, name)
			return nil
		},
	}
}
