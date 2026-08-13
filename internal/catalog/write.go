package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// catalogFile は catalog の書き込み先。os.File のうち Write が使う操作だけを
// 切り出してある（テストが「書き込み途中で停止」を注入するための継ぎ目。
// generation_test.go の failing writer を参照）。
type catalogFile interface {
	io.Writer
	Sync() error
	Close() error
}

// openCatalogFile は世代を構成するファイルを最終名で開く。**一時名 → rename は
// 使わない**（docs/storage.md §2: S3/FUSE にアトミック rename は無い）。公開点は
// manifest を最後に書き終えることであって、名前の差し替えではない。
var openCatalogFile = func(path string) (catalogFile, error) {
	return os.Create(path)
}

// GenerationName は UTC 時刻から世代ディレクトリ名を作る。
// 形: catalog-YYYYMMDDTHHMMSSZ
func GenerationName(t time.Time) string {
	return FilenamePrefix + t.UTC().Format("20060102T150405Z")
}

// Dir は media_dir 配下の catalog ディレクトリパスを返す。
func Dir(mediaDir string) string {
	return filepath.Join(mediaDir, Subdir)
}

// Write は Document を新しい世代ディレクトリとして media_dir/catalog/ に書き、
// 古い世代を刈る。返り値は書いた世代ディレクトリのパス。
//
// 手順（docs/storage.md §8「世代の完成判定」）:
//
//  1. 既存の世代名と衝突しない世代ディレクトリを作る（**既存世代を上書きしない**）
//  2. 本体 catalog.json を最終名へ一発書き → fsync
//  3. **最後に** manifest.json を一発書き → fsync。これが完成宣言 = 公開点
//  4. Prune
//
// 途中で失敗しても書きかけの世代ディレクトリは消さない。プロセスが落ちる形の
// 停止では後始末が走らない以上、後始末を正しさの前提にできないため（掃除は
// Prune のルールに一本化する。docs/storage.md §8「不完全世代の保持と掃除」）。
func Write(mediaDir string, doc *Document, keep int) (string, error) {
	if doc == nil {
		return "", fmt.Errorf("document is nil")
	}
	if doc.Version <= 0 {
		return "", fmt.Errorf("document version is zero")
	}
	if keep <= 0 {
		keep = DefaultKeep
	}

	dir := Dir(mediaDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating catalog dir: %w", err)
	}

	name, err := uniqueGenerationName(dir, GenerationName(doc.ExportedAt))
	if err != nil {
		return "", err
	}
	genDir := filepath.Join(dir, name)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return "", fmt.Errorf("creating generation dir: %w", err)
	}

	docFile, err := writeGenerationFile(filepath.Join(genDir, DocumentFilename), DocumentFilename, doc)
	if err != nil {
		return genDir, err
	}

	manifest := &Manifest{
		ManifestVersion: ManifestVersion,
		Generation:      name,
		SchemaVersion:   doc.Version,
		ExportedAt:      doc.ExportedAt.UTC(),
		Document:        DocumentFilename,
		Files:           []ManifestFile{docFile},
	}
	if _, err := writeGenerationFile(filepath.Join(genDir, ManifestFilename), ManifestFilename, manifest); err != nil {
		return genDir, err
	}

	if err := Prune(dir, keep); err != nil {
		return genDir, fmt.Errorf("pruning catalog: %w", err)
	}
	return genDir, nil
}

// writeGenerationFile は v を JSON で path へ一発書きし、サイズと sha256 を返す。
func writeGenerationFile(path, name string, v any) (ManifestFile, error) {
	f, err := openCatalogFile(path)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("creating %s: %w", name, err)
	}
	h := sha256.New()
	counter := &countingWriter{}
	enc := json.NewEncoder(io.MultiWriter(f, h, counter))
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		return ManifestFile{}, fmt.Errorf("encoding %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return ManifestFile{}, fmt.Errorf("syncing %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return ManifestFile{}, fmt.Errorf("closing %s: %w", name, err)
	}
	return ManifestFile{
		Name:      name,
		SizeBytes: counter.n,
		SHA256:    hex.EncodeToString(h.Sum(nil)),
	}, nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// uniqueGenerationName は base から始めて、まだ存在しない世代名を返す。
// 同じ秒に 2 回書くことになっても既存世代を上書きしない（連番を足す）。
func uniqueGenerationName(dir, base string) (string, error) {
	for i := 1; i <= 100; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		_, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking generation %q: %w", name, err)
		}
	}
	return "", fmt.Errorf("too many generations named %q", base)
}

// snapshot は catalog ディレクトリの 1 エントリ（世代ディレクトリ or 旧形式の
// フラットなファイル）。
type snapshot struct {
	name string
	// key は並べ替えキー。旧形式は拡張子を落として世代名と同じ土俵で比べる。
	key string
	// generation なら世代ディレクトリ、そうでなければ旧形式のフラットファイル。
	generation bool
}

// sortSnapshotsDesc は新しい順（辞書順降順）に並べる。時刻が同着なら世代
// ディレクトリを先（新しい側）に置く。
func sortSnapshotsDesc(s []snapshot) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].key != s[j].key {
			return s[i].key > s[j].key
		}
		return s[i].generation && !s[j].generation
	})
}

// scanSnapshots は catalogDir の世代ディレクトリと旧形式ファイルを新しい順に返す。
// 残骸（catalog-*.tmp）の名前も別に返す。
func scanSnapshots(catalogDir string) (snaps []snapshot, stale []string, err error) {
	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading catalog dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, FilenamePrefix) {
			continue
		}
		switch {
		case e.IsDir():
			snaps = append(snaps, snapshot{name: name, key: name, generation: true})
		case strings.HasSuffix(name, ".tmp"):
			// 世代ディレクトリ導入前の書き込み方式（一時名 → rename）の残骸。
			stale = append(stale, name)
		case isCatalogJSON(name):
			snaps = append(snaps, snapshot{name: name, key: strings.TrimSuffix(name, ".json")})
		}
	}
	sortSnapshotsDesc(snaps)
	return snaps, stale, nil
}

// Prune は catalogDir を掃除する（docs/storage.md §8「不完全世代の保持と掃除」）。
//
//   - 使える世代（完成世代 + 旧形式のフラットファイル）を新しい順に keep 件残し、
//     それより古いものを消す
//   - 不完全な世代は「それより新しい使える世代がある」ときだけ消す。最新側の
//     不完全世代は**進行中のエクスポートかもしれない**ので残す
//   - 旧方式の残骸（catalog-*.tmp）は消す
//
// catalog/ 以外は触らない。catalog- で始まらないファイルにも触らない。
func Prune(catalogDir string, keep int) error {
	if keep <= 0 {
		keep = DefaultKeep
	}

	snaps, stale, err := scanSnapshots(catalogDir)
	if err != nil {
		return err
	}
	for _, name := range stale {
		_ = os.Remove(filepath.Join(catalogDir, name))
	}

	var doomed []string
	// 保持の順位は SelectLatest の選択順と同じにする（完成世代が先、旧形式は
	// 最後）。時刻順だけで刈ると、検証できない旧形式ファイルのために検証済みの
	// 完成世代を消しうる。
	var generations, legacies []string
	var usableSeen bool
	for _, s := range snaps {
		usable := true
		if s.generation {
			if _, err := VerifyGeneration(filepath.Join(catalogDir, s.name)); err != nil {
				usable = false
			}
		}
		if !usable {
			// 不完全世代: 時刻順で新しい側に使えるものが 1 つでもあれば消す。
			// 無ければ進行中のエクスポートかもしれないので残す。
			if usableSeen {
				doomed = append(doomed, s.name)
			}
			continue
		}
		usableSeen = true
		if s.generation {
			generations = append(generations, s.name)
		} else {
			legacies = append(legacies, s.name)
		}
	}

	retained := make([]string, 0, len(generations)+len(legacies))
	retained = append(retained, generations...)
	retained = append(retained, legacies...)
	if len(retained) > keep {
		doomed = append(doomed, retained[keep:]...)
	}

	for _, name := range doomed {
		if err := os.RemoveAll(filepath.Join(catalogDir, name)); err != nil {
			return fmt.Errorf("removing catalog %q: %w", name, err)
		}
	}
	return nil
}

// Selection は rescue が選んだ catalog スナップショット。
type Selection struct {
	// DocumentPath は読むべき catalog 本体 JSON のパス。
	DocumentPath string
	// Generation は選んだ世代ディレクトリ名。旧形式なら空。
	Generation string
	// Manifest は選んだ世代の完成宣言。旧形式なら nil。
	Manifest *Manifest
	// Legacy は manifest を持たない旧形式のフラットファイルを選んだことを示す
	// （**完成を検証できていない**最後の手段。docs/storage.md §8）。
	Legacy bool
	// Rejected は選ばずに飛ばした世代とその理由（新しい順）。黙って古い世代へ
	// 落ちないよう、呼び出し側が必ず報告できるように返す。
	Rejected []RejectedSnapshot
}

// RejectedSnapshot は完成判定に落ちた世代（または読めなかった旧形式ファイル）。
type RejectedSnapshot struct {
	Name   string
	Reason string
}

// SelectLatest は media_dir/catalog/ から**最新の完成世代**を選ぶ。
//
// 新しい順に完成判定（VerifyGeneration）を掛け、最初に通ったものを返す。最新が
// 不完全なら 1 つ前の完成世代へ落ちる。完成世代が 1 つも無ければ、manifest を
// 持たない旧形式のフラットファイル（世代ディレクトリ導入前の出力）を新しい順に
// 読めるだけ読む。
//
// 使えるものが 1 つも無ければ os.ErrNotExist を返すが、**Selection 自体は返す**
// （飛ばした世代の理由を呼び出し側が報告できるようにする。「catalog が無い」と
// 「catalog はあるが全部不完全だった」を混同させない）。
func SelectLatest(mediaDir string) (*Selection, error) {
	dir := Dir(mediaDir)
	snaps, _, err := scanSnapshots(dir)
	if err != nil {
		return nil, err
	}

	sel := &Selection{}
	// 1 巡目: 完成世代だけを見る。旧形式は検証できないので後回しにする。
	var legacy []snapshot
	for _, s := range snaps {
		if !s.generation {
			legacy = append(legacy, s)
			continue
		}
		path := filepath.Join(dir, s.name)
		m, err := VerifyGeneration(path)
		if err != nil {
			sel.Rejected = append(sel.Rejected, RejectedSnapshot{Name: s.name, Reason: err.Error()})
			continue
		}
		sel.DocumentPath = filepath.Join(path, m.Document)
		sel.Generation = s.name
		sel.Manifest = m
		return sel, nil
	}

	// 2 巡目: 旧形式。parse できることしか確かめられない。
	for _, s := range legacy {
		path := filepath.Join(dir, s.name)
		if _, err := Load(path); err != nil {
			sel.Rejected = append(sel.Rejected, RejectedSnapshot{Name: s.name, Reason: err.Error()})
			continue
		}
		sel.DocumentPath = path
		sel.Legacy = true
		return sel, nil
	}

	return sel, os.ErrNotExist
}

// Load は path から Document を読む。
func Load(path string) (*Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening catalog: %w", err)
	}
	defer func() { _ = f.Close() }()

	var doc Document
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding catalog: %w", err)
	}
	if doc.Version == 0 {
		// 旧形を許さない。明示的に version を書く。
		return nil, fmt.Errorf("catalog version missing or zero in %q", path)
	}
	if doc.Version > Version {
		return nil, fmt.Errorf("catalog version %d in %q is newer than supported %d",
			doc.Version, path, Version)
	}
	return &doc, nil
}

func isCatalogJSON(name string) bool {
	return strings.HasPrefix(name, FilenamePrefix) && strings.HasSuffix(name, ".json")
}
