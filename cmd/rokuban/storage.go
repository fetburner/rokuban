package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
store が空の観測行と取り込み進捗行をこの置き場へ割り当て、既存コピーの
パスから派生世代を補正する。すべての論理資産にコピーが必要である。

コピーの無い論理資産がある場合、この版では物理コピーを復元できない。
互換列を削除する前に、互換列を使える版で storage adopt を実行する。

目印が無い root に新しく目印を作る前に、DB にこの置き場のコピー行が
あるかを確かめる。あれば、マウント漏れか別のボリュームの可能性があるので
目印を書かずにエラーにする。同名の目印が既にあれば、そのまま成功する。`,
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
				return fmt.Errorf("store name %q does not match the fs store in storage.stores %q", name, cfg.Storage.MediaStoreName)
			}
			store, ok := cfg.Storage.Stores[name]
			if !ok {
				return fmt.Errorf("store name %q is not configured in storage.stores", name)
			}
			if store.Kind != "fs" {
				return fmt.Errorf("storage adopt requires an fs store, but storage.stores.%s.kind is %q", name, store.Kind)
			}
			markerPath := filepath.Join(cfg.Storage.MediaDir, mediapath.StoreMarkerFileName)
			_, statErr := os.Lstat(markerPath)
			markerExists := statErr == nil
			if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("checking %s: %w", markerPath, statErr)
			}
			if markerExists {
				// 異なる名前の目印は DB に触れる前に拒む。
				if err := mediapath.CheckStoreMarker(cfg.Storage.MediaDir, name); err != nil {
					return err
				}
			}
			pool, err := db.NewPool(cmd.Context(), cfg.DB, nil, 0)
			if err != nil {
				return err
			}
			defer pool.Close()
			media := pgstore.NewMedia(pool, name, "") // 照合だけで削除の判定はしない
			if err := media.RequireMediaAssetCopiesAssigned(cmd.Context()); err != nil {
				return err
			}
			if !markerExists {
				has, err := media.HasStoreCopies(cmd.Context())
				if err != nil {
					return err
				}
				if has {
					return fmt.Errorf("store %q already has media asset copies in the database but %s does not exist: the media root may not be mounted or may be a different volume; not writing a marker", name, markerPath)
				}
				if err := mediapath.WriteStoreMarker(cfg.Storage.MediaDir, name); err != nil {
					return err
				}
			}
			if err := media.AdoptUnassignedStoreRows(cmd.Context()); err != nil {
				return fmt.Errorf("adopting unassigned media store rows: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s for store %q\n", mediapath.StoreMarkerFileName, name)
			return nil
		},
	}
}
