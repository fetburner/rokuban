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
		Long: `置き場の root（storage.stores.<name>.root、無ければ media_dir）に .rokuban-store を作成し、置き場名を書き込む。
store が空の観測行と取り込み進捗行をこの置き場へ割り当てる。コピー行は作らない。

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
			store, ok := cfg.Storage.Stores[name]
			if !ok {
				return fmt.Errorf("store name %q is not configured in storage.stores", name)
			}
			if store.Kind != "fs" {
				return fmt.Errorf("storage adopt requires an fs store, but storage.stores.%s.kind is %q", name, store.Kind)
			}
			roots, err := cfg.Storage.StoreRoots([]string{name})
			if err != nil {
				return err
			}
			root := roots[name]
			markerPath := filepath.Join(root, mediapath.StoreMarkerFileName)
			_, statErr := os.Lstat(markerPath)
			markerExists := statErr == nil
			if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("checking %s: %w", markerPath, statErr)
			}
			if markerExists {
				// 異なる名前の目印は DB に触れる前に拒む。
				if err := mediapath.CheckStoreMarker(root, name); err != nil {
					return err
				}
			}
			pool, err := db.NewPool(cmd.Context(), cfg.DB, nil, 0, 0)
			if err != nil {
				return err
			}
			defer pool.Close()
			// スキーマが埋め込みの最新 migration でなければ、目印も行も触らずに失敗する。
			// Job はこの失敗で再試行され、マイグレーションの完了を待つ。
			if err := db.CheckMigrationsCurrent(cmd.Context(), cfg.DB.DSN()); err != nil {
				return fmt.Errorf("storage adopt: %w", err)
			}
			media := pgstore.NewMedia(pool, name, "") // 照合だけで削除の判定はしない
			if !markerExists {
				has, err := media.HasStoreCopies(cmd.Context())
				if err != nil {
					return err
				}
				if has {
					return fmt.Errorf("store %q already has media asset copies in the database but %s does not exist: the media root may not be mounted or may be a different volume; not writing a marker", name, markerPath)
				}
				if err := mediapath.WriteStoreMarker(root, name); err != nil {
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
