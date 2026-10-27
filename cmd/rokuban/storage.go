package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store/pgstore"
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
		Short: "置き場の目印を書き、既存行を割り当てる",
		Args:  cobra.ExactArgs(1),
		Long: `media_dir の root に .rokuban-store を作成し、置き場名を書き込む。
store が空の既存コピー・観測行を、この置き場へ割り当てる。`,
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
			pool, err := db.NewPool(cmd.Context(), cfg.DB, nil, 0)
			if err != nil {
				return err
			}
			defer pool.Close()
			if err := pgstore.NewMedia(pool, name).AdoptUnassignedStoreRows(cmd.Context()); err != nil {
				return fmt.Errorf("adopting unassigned media store rows: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s for store %q\n", mediapath.StoreMarkerFileName, name)
			return nil
		},
	}
}
