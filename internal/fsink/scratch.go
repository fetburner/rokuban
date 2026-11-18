package fsink

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewScratchDir は kind の下に、prefix を持つ試行固有のディレクトリを作る。
// ロゴ候補・thumbnail・seek tiles・encode・transfer の作業ファイルを試行間で分離する。
// TestNewScratchDirIsUniqueForSameRecording が同じ prefix での一意性を検査する。
func NewScratchDir(scratchRoot, kind, prefix string) (string, error) {
	if scratchRoot == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	root := filepath.Join(scratchRoot, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch root: %w", kind, err)
	}
	return os.MkdirTemp(root, prefix)
}
