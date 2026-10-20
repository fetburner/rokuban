package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/fetburner/rokuban/internal/medialock"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

// RescueResult は復元件数と、選択した catalog 世代の情報をまとめる。
type RescueResult struct {
	store.RestoreResult
	// CatalogPath は復元に使った文書のパス。
	CatalogPath string
	// Generation は復元に使った世代ディレクトリ名。
	Generation string
	// RejectedSnapshots は完成判定に落ちて飛ばした世代（新しい順）。
	RejectedSnapshots []RejectedSnapshot
	// SkippedFilesWithoutSitePrefix は site を決められず登録しなかった走査対象の件数。
	SkippedFilesWithoutSitePrefix int
}

// RescueLatest は media_dir/catalog/ の最新の完成世代を復元する。
// 完成世代がなければ media_dir を走査して、認識可能な動画ファイルを登録する。
// Catalog の DB 操作は Catalog が、ストレージ走査からの登録は MediaPublish が行う。
func RescueLatest(
	ctx context.Context,
	catalogStore store.Catalog,
	mediaPublish store.MediaPublish,
	mediaDir string,
	registrySites []string,
) (*RescueResult, error) {
	sel, err := SelectLatest(mediaDir)
	if sel != nil {
		for _, rejected := range sel.Rejected {
			slog.Warn("rescue: skipping incomplete catalog generation",
				"generation", rejected.Name, "reason", rejected.Reason)
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			result, scanErr := rescueStorage(ctx, mediaPublish, mediaDir, registrySites)
			if scanErr != nil {
				return nil, scanErr
			}
			if sel != nil {
				result.RejectedSnapshots = sel.Rejected
			}
			return result, nil
		}
		return nil, err
	}
	result, err := RescueFile(ctx, catalogStore, mediaDir, sel.DocumentPath)
	if err != nil {
		return nil, err
	}
	result.Generation = sel.Generation
	result.RejectedSnapshots = sel.Rejected
	return result, nil
}

// RescueFile は path の catalog JSON を読み、mediaDir のファイルをロックしたまま
// store に渡して冪等 upsert する。存在しないファイルの行は deleted として復元する。
func RescueFile(ctx context.Context, catalogStore store.Catalog, mediaDir, path string) (*RescueResult, error) {
	doc, err := Load(path)
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(doc.MediaAssets))
	for _, asset := range doc.MediaAssets {
		if asset.State != "deleted" {
			paths = append(paths, asset.RelPath)
		}
	}
	release, err := medialock.LockPaths(ctx, mediaDir, paths)
	if err != nil {
		return nil, fmt.Errorf("locking rescue assets: %w", err)
	}
	defer func() { _ = release() }()

	missing := make(map[int64]struct{})
	for _, asset := range doc.MediaAssets {
		if asset.State == "deleted" {
			continue
		}
		fullPath, err := mediapath.Resolve(mediaDir, asset.RelPath)
		if err != nil {
			return nil, fmt.Errorf("resolving rescue asset %q: %w", asset.RelPath, err)
		}
		info, err := os.Lstat(fullPath)
		if errors.Is(err, os.ErrNotExist) {
			missing[asset.ID] = struct{}{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking rescue asset %q: %w", asset.RelPath, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("rescue asset %q is not a regular file", asset.RelPath)
		}
	}

	result, err := catalogStore.Restore(ctx, doc, store.RestoreOptions{MissingAssetIDs: missing})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("restoring catalog returned no result")
	}
	return &RescueResult{RestoreResult: *result, CatalogPath: path}, nil
}
