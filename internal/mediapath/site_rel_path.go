package mediapath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SiteRelPathPrefix は原本 rel_path の site 名前空間の固定 1 段目
// （docs/storage/contract.md §「原本の sites/{site}/ 前置」）。書き手の
// SiteRelPath と読み手の catalog.classifySiteForRescuedFile がこの定数を共有する。
const SiteRelPathPrefix = "sites/"

// SiteRelPath は原本の rel_path に site 名前空間を前置する。
// rel_path がファイルを指さない場合や予約名ならエラーにする。
// TestSiteRelPath は前置と拒否する path を固定する。
func SiteRelPath(site, relPath string) (string, error) {
	if relPath == "" || relPath == "." || relPath == "/" || strings.HasSuffix(relPath, "/") {
		return "", fmt.Errorf("rel_path %q does not name a file", relPath)
	}
	base := filepath.Base(relPath)
	if base == "." || base == ".." {
		return "", fmt.Errorf("rel_path %q does not name a file", relPath)
	}
	if IsIngestTempFile(base) || IsStorageTransferTempFile(base) ||
		IsStorageTransferCheckpointFile(base) || IsMediaRelPathLockFile(base) ||
		IsEncodeTempFile(base) || IsGeneratedAssetTempFile(base) {
		return "", fmt.Errorf("rel_path %q uses reserved storage filename %q", relPath, base)
	}
	return SiteRelPathPrefix + site + "/" + relPath, nil
}
