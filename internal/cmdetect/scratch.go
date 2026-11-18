package cmdetect

import (
	"fmt"
	"os"
	"path/filepath"
)

// newDomainScratchDir はドメインキーに対応する固定 scratch directory を、同じキーの
// 残骸があれば消して作り直す。プロセス死の残骸を回収する仕組みではない。
// CM 検出の key は attempt_count を含み、死後の次の開始で番号が進むので、消せるのは
// 停止キャンセルで同じ番号に戻った場合だけである。
func newDomainScratchDir(scratchRoot, kind, key string) (string, error) {
	if scratchRoot == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	root := filepath.Join(scratchRoot, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch root: %w", kind, err)
	}
	jobDir := filepath.Join(root, key)
	if err := os.RemoveAll(jobDir); err != nil {
		return "", fmt.Errorf("cleaning previous %s scratch directory: %w", kind, err)
	}
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch directory: %w", kind, err)
	}
	return jobDir, nil
}
