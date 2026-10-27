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

// RescueResult は rescue で復元した件数のサマリ。復元件数は store.RestoreResult が持つ。
// store.RestoreResult.MissingMediaFiles は、ストレージ走査では観測後に消えた
// 登録前の候補も数える。
type RescueResult struct {
	store.RestoreResult
	// CatalogPath は復元に使った文書のパス。
	CatalogPath string
	// Generation は復元に使った世代ディレクトリ名（docs/storage.md §8）。
	Generation string
	// RejectedSnapshots は完成判定に落ちて飛ばした世代（新しい順）。空でなければ
	// 「最新に見えた世代を飛ばして古い世代へ落ちた」ということなので、黙って
	// 成功させずに呼び出し側が報告する。
	RejectedSnapshots []RejectedSnapshot

	// SkippedFilesWithoutSitePrefix はストレージ走査（catalog が 1 世代も
	// 無いときの rescueStorage）で見つかったが `sites/{site}/` 前置が無く
	// site を決められないため登録しなかったファイルの件数。DB を喪失した後の
	// rescue で唯一この件数だけが復元漏れを示す（slog の Warn は運用者の目に
	// 触れるとは限らない）ので、呼び出し側のサマリ表示に必ず出す。
	SkippedFilesWithoutSitePrefix int
}

// RescueLatest は media_dir/catalog/ の**最新の完成世代**を読んで DB に冪等
// upsert する（完成判定は SelectLatest / VerifyGeneration。docs/storage.md §8）。
// Catalog の DB 操作は Catalog が、ストレージ走査からの登録は MediaPublish が行う。
//
// 最新世代が不完全・checksum 不一致なら 1 つ前の完成世代へ落ちる。完成世代が
// 1 つも無ければ media_dir を走査して `sites/{site}/` 前置を持つ認識可能な動画
// ファイルを素の asset として in-place 登録する。前置の無いファイルは登録しない。
// どの経路もファイル本体はコピー・変更しない。
//
// registrySites は `mirakcs:` レジストリの site 名一覧。ストレージ走査で
// `sites/{site}/` 前置から site を読んだとき、その site がレジストリに
// 無ければ typo/ゴミディレクトリの疑いがあるとして Warn で目立たせる
// （classifySiteForRescuedFile 参照）。catalog JSON からの復元では使わない。
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
			// 「catalog が 1 つも無い」と「あったが全部不完全だった」を
			// 混同させない: 飛ばした世代は結果に載せて呼び出し側に報告させる。
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
//
// 書き込み順は store.Catalog.Restore の実装（pgstore.Catalog.Restore）にあり、
// 全部 1 トランザクションで、途中失敗なら何も残さない。
func RescueFile(ctx context.Context, catalogStore store.Catalog, mediaDir, path string) (*RescueResult, error) {
	doc, err := Load(path)
	if err != nil {
		return nil, err
	}
	storeName, err := mediapath.ReadStoreMarker(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("reading media store marker: %w", err)
	}
	for i := range doc.MediaAssets {
		asset := &doc.MediaAssets[i]
		if asset.Store == "" {
			asset.Store = storeName
		}
		if !mediapath.ValidStoreName(asset.Store) {
			return nil, fmt.Errorf("catalog media asset %d has invalid store name %q", asset.ID, asset.Store)
		}
	}

	// Acquire file locks before DB row locks and retain them through commit.
	// deleted の行は active 行を作らない（ファイルも見ない）ので、ロック対象から外す。
	// 墓石は運用とともに増えるため、ロック数を live な行に抑える。
	paths := make([]string, 0, len(doc.MediaAssets))
	for _, asset := range doc.MediaAssets {
		if asset.State != "deleted" && asset.Store == storeName {
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
		if asset.State == "deleted" || asset.Store != storeName {
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
