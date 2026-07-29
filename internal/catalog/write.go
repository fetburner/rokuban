package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Filename は UTC 時刻から catalog ファイル名を作る。
// 形: catalog-YYYYMMDDTHHMMSSZ.json
func Filename(t time.Time) string {
	return FilenamePrefix + t.UTC().Format("20060102T150405Z") + ".json"
}

// Dir は media_dir 配下の catalog ディレクトリパスを返す。
func Dir(mediaDir string) string {
	return filepath.Join(mediaDir, Subdir)
}

// Write は Document を media_dir/catalog/ に書き、古い世代を刈る。
//
// 手順: 一時ファイルへ一発書き → rename。S3 FUSE では rename が非アトミック
// になりうるが、DB が真実の座なので許容する（docs/storage.md §3 / §8）。
// 返り値は書き込んだ最終パス。
func Write(mediaDir string, doc *Document, keep int) (string, error) {
	if doc == nil {
		return "", fmt.Errorf("document is nil")
	}
	if keep <= 0 {
		keep = DefaultKeep
	}

	dir := Dir(mediaDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating catalog dir: %w", err)
	}

	name := Filename(doc.ExportedAt)
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return "", fmt.Errorf("creating temp catalog file: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("encoding catalog json: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("syncing catalog file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("closing catalog file: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("renaming catalog file: %w", err)
	}

	if err := Prune(dir, keep); err != nil {
		return final, fmt.Errorf("pruning catalog: %w", err)
	}
	return final, nil
}

// Prune は catalogDir 配下の catalog-*.json を新しい順に並べ、keep 件を超えた
// 古いファイルだけを削除する。.tmp 残骸も削除する。catalog/ 以外は触らない。
func Prune(catalogDir string, keep int) error {
	if keep <= 0 {
		keep = DefaultKeep
	}

	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading catalog dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 書きかけの残骸を掃除
		if strings.HasSuffix(name, ".tmp") && strings.HasPrefix(name, FilenamePrefix) {
			_ = os.Remove(filepath.Join(catalogDir, name))
			continue
		}
		if isCatalogJSON(name) {
			names = append(names, name)
		}
	}

	// ファイル名に UTC 時刻が入っているので辞書順降順 = 新しい順。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for i, name := range names {
		if i < keep {
			continue
		}
		if err := os.Remove(filepath.Join(catalogDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing old catalog %q: %w", name, err)
		}
	}
	return nil
}

// LatestPath は media_dir/catalog/ 内の最新 catalog JSON の絶対パスを返す。
// 見つからなければ ("", os.ErrNotExist)。
func LatestPath(mediaDir string) (string, error) {
	dir := Dir(mediaDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("reading catalog dir: %w", err)
	}

	var latest string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isCatalogJSON(name) {
			continue
		}
		if name > latest {
			latest = name
		}
	}
	if latest == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(dir, latest), nil
}

// Load は path から Document を読む。
func Load(path string) (*Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening catalog: %w", err)
	}
	defer f.Close()

	var doc Document
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding catalog: %w", err)
	}
	if doc.Version == 0 {
		// 旧形を許さない。明示的に version を書く。
		return nil, fmt.Errorf("catalog version missing or zero in %q", path)
	}
	return &doc, nil
}

func isCatalogJSON(name string) bool {
	return strings.HasPrefix(name, FilenamePrefix) && strings.HasSuffix(name, ".json")
}
