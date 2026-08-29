package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// catalogFile は catalog の書き込み先。os.File のうち Write が使う操作だけを
// 切り出してある（テストが「書き込み途中で停止」と「書き込み順序の観測」を
// 注入するための継ぎ目。generation_test.go の stoppingFile / recordingOpener
// を参照）。
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

	docSize, docSHA256, err := writeGenerationFile(filepath.Join(genDir, DocumentFilename), DocumentFilename, doc)
	if err != nil {
		return genDir, err
	}

	manifest := &Manifest{
		ManifestVersion: ManifestVersion,
		Generation:      name,
		SchemaVersion:   doc.Version,
		ExportedAt:      doc.ExportedAt.UTC(),
		Document:        DocumentFilename,
		SizeBytes:       docSize,
		SHA256:          docSHA256,
	}
	if _, _, err := writeGenerationFile(filepath.Join(genDir, ManifestFilename), ManifestFilename, manifest); err != nil {
		return genDir, err
	}

	if err := Prune(dir, keep); err != nil {
		return genDir, fmt.Errorf("pruning catalog: %w", err)
	}
	return genDir, nil
}

// writeGenerationFile は v を JSON で path へ一発書きし、サイズと sha256 を返す。
func writeGenerationFile(path, name string, v any) (sizeBytes int64, sha256Hex string, err error) {
	f, err := openCatalogFile(path)
	if err != nil {
		return 0, "", fmt.Errorf("creating %s: %w", name, err)
	}
	h := sha256.New()
	counter := &countingWriter{}
	enc := json.NewEncoder(io.MultiWriter(f, h, counter))
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		return 0, "", fmt.Errorf("encoding %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, "", fmt.Errorf("syncing %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return 0, "", fmt.Errorf("closing %s: %w", name, err)
	}
	return counter.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// uniqueGenerationName は base から始めて、まだ存在しない世代名を返す。
// 同じ秒に 2 回書くことになっても既存世代を上書きしない（連番を足す）。
//
// 連番は**ゼロ詰め 2 桁**にする。辞書順が「新しい順」の唯一の根拠なので、
// `-2` と `-10` を並べると 10 本目が 2 本目より古い側に落ちる（`-02` < `-10`）。
// 99 本を超えたら名前を作らずエラーにする（黙って順序の壊れた名前を作らない）。
func uniqueGenerationName(dir, base string) (string, error) {
	for i := 1; i <= 99; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%02d", base, i)
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

// scanSnapshots は catalogDir の世代ディレクトリを新しい順（名前の辞書順降順 ---
// 名前が `catalog-<UTC 時刻>` なので時刻順と一致する）に返す。残骸
// （catalog-*.tmp）の名前も別に返す。
func scanSnapshots(catalogDir string) (names []string, stale []string, err error) {
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
			names = append(names, name)
		case strings.HasSuffix(name, ".tmp"):
			// 書き込み途中（一時名 → rename）の残骸。
			stale = append(stale, name)
		}
	}
	slices.Sort(names)
	slices.Reverse(names)
	return names, stale, nil
}

// Prune は catalogDir を掃除する（docs/storage.md §8「不完全世代の保持と掃除」）。
//
//   - 完成世代を新しい順に keep 件残し、溢れた分を消す
//   - 不完全な世代は「時刻順でそれより新しい完成世代がある」ときだけ消す。
//     最新側の不完全世代は**進行中のエクスポートかもしれない**ので残す
//   - 書き込み途中の残骸（catalog-*.tmp）は消す
//
// **Prune を呼ぶのは Write の成功パスだけ**（エクスポートが失敗し続ける間は
// 掃除も走らない）。docs/storage.md §8 に同じ但し書きがある。
//
// catalog/ 以外は触らない。catalog- で始まらないファイルにも触らない。
func Prune(catalogDir string, keep int) error {
	if keep <= 0 {
		keep = DefaultKeep
	}

	names, stale, err := scanSnapshots(catalogDir)
	if err != nil {
		return err
	}
	for _, name := range stale {
		_ = os.Remove(filepath.Join(catalogDir, name))
	}

	var doomed, complete []string
	for _, name := range names {
		if _, err := VerifyGeneration(filepath.Join(catalogDir, name)); err != nil {
			// 不完全世代: 時刻順で新しい側に完成世代が 1 つでもあれば消す。
			// 無ければ進行中のエクスポートかもしれないので残す。
			if len(complete) > 0 {
				doomed = append(doomed, name)
			}
			continue
		}
		complete = append(complete, name)
	}
	if len(complete) > keep {
		doomed = append(doomed, complete[keep:]...)
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
	// Generation は選んだ世代ディレクトリ名。
	Generation string
	// Manifest は選んだ世代の完成宣言。
	Manifest *Manifest
	// Rejected は選ばずに飛ばした世代とその理由（新しい順）。黙って古い世代へ
	// 落ちないよう、呼び出し側が必ず報告できるように返す。
	Rejected []RejectedSnapshot
}

// RejectedSnapshot は完成判定に落ちた世代。
type RejectedSnapshot struct {
	Name   string
	Reason string
}

// SnapshotStatus は catalog/ の 1 世代を完成判定に掛けた結果。
type SnapshotStatus struct {
	// Name は世代ディレクトリ名。
	Name string
	// Complete は完成判定（manifest + サイズ + sha256）を通ったこと。
	Complete bool
	// Reason は Complete が false のときの理由。
	Reason string
	// Manifest は完成世代の manifest（不完全なら nil）。
	Manifest *Manifest
}

// ListSnapshots は media_dir/catalog/ の全世代を新しい順に並べ、1 つずつ
// 完成判定に掛けた結果を返す。**DB には一切触らない。**
//
// 最初に Complete が true になった世代が、rescue が選ぶものと一致する
// （TestListSnapshots_FirstCompleteMatchesSelectLatest）。
func ListSnapshots(mediaDir string) ([]SnapshotStatus, error) {
	dir := Dir(mediaDir)
	names, _, err := scanSnapshots(dir)
	if err != nil {
		return nil, err
	}

	out := make([]SnapshotStatus, 0, len(names))
	for _, name := range names {
		st := SnapshotStatus{Name: name, Complete: true}
		m, err := VerifyGeneration(filepath.Join(dir, name))
		if err != nil {
			st.Complete = false
			st.Reason = err.Error()
		}
		st.Manifest = m
		out = append(out, st)
	}
	return out, nil
}

// SelectLatest は media_dir/catalog/ から**最新の完成世代**を選ぶ。
//
// 新しい順に完成判定（VerifyGeneration）を掛け、最初に通ったものを返す。最新が
// 不完全なら 1 つ前の完成世代へ落ちる。
//
// 使えるものが 1 つも無ければ os.ErrNotExist を返すが、**Selection 自体は返す**
// （飛ばした世代の理由を呼び出し側が報告できるようにする。「catalog が無い」と
// 「catalog はあるが全部不完全だった」を混同させない）。
func SelectLatest(mediaDir string) (*Selection, error) {
	statuses, err := ListSnapshots(mediaDir)
	if err != nil {
		return nil, err
	}

	dir := Dir(mediaDir)
	sel := &Selection{}
	for _, st := range statuses {
		if !st.Complete {
			sel.Rejected = append(sel.Rejected, RejectedSnapshot{Name: st.Name, Reason: st.Reason})
			continue
		}
		sel.DocumentPath = filepath.Join(dir, st.Name, st.Manifest.Document)
		sel.Generation = st.Name
		sel.Manifest = st.Manifest
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
