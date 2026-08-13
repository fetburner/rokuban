package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ManifestVersion は manifest 自体の形式版。manifest の読み方を壊す変更で上げる
// （catalog document の版 = Version とは別に持つ）。
const ManifestVersion = 1

const (
	// DocumentFilename は世代ディレクトリ内の catalog 本体のファイル名。
	DocumentFilename = "catalog.json"

	// ManifestFilename は世代の完成宣言のファイル名。**世代データを全部書き
	// 終えた後に最後の一発書きで置く**（docs/storage.md §8「世代の完成判定」）。
	// これが公開点であって、rename でも `latest` の差し替えでもない。
	ManifestFilename = "manifest.json"
)

// Manifest は 1 世代の完成宣言。世代ディレクトリの `manifest.json` に置く。
//
// 存在するだけでは完成を意味しない: VerifyGeneration が files の全項目を
// サイズと sha256 で照合して初めて完成世代になる（docs/storage.md §8）。
type Manifest struct {
	// ManifestVersion はこのファイル自体の形式版。
	ManifestVersion int `json:"manifestVersion"`
	// Generation は世代ディレクトリ名。ディレクトリ名と一致しなければ、
	// 別の世代からコピーされた manifest とみなして不完全に倒す。
	Generation string `json:"generation"`
	// SchemaVersion は Document.Version。読み手が自分より新しい世代を
	// 「読めるふりをして誤読する」ことを防ぐために manifest 側にも持つ
	// （本体を開かずに判定できる）。
	SchemaVersion int `json:"schemaVersion"`
	// ExportedAt は Document.ExportedAt と同じ時刻。
	ExportedAt time.Time `json:"exportedAt"`
	// Document は本体ファイル名。Files に載っていなければ不完全。
	Document string `json:"document"`
	// Files は世代を構成するファイルの一覧（manifest 自身は含まない）。
	Files []ManifestFile `json:"files"`
}

// ManifestFile は世代を構成する 1 ファイルの完成条件。
type ManifestFile struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

// VerifyGeneration は世代ディレクトリが完成世代かを判定し、manifest を返す。
//
// 完成条件（docs/storage.md §8「世代の完成判定」。1 つでも欠ければ error）:
//
//  1. manifest.json が存在し、JSON として最後まで parse できる
//  2. manifestVersion がこのバイナリの理解する版以下
//  3. schemaVersion がこのバイナリの読める catalog 版以下
//  4. generation が世代ディレクトリ名と一致する
//  5. document が files[] に載っている
//  6. files[] の全項目が存在し、サイズと sha256 が両方一致する
//
// rename のアトミック性には依存しない。判定の材料は世代ディレクトリの中身だけ。
func VerifyGeneration(genDir string) (*Manifest, error) {
	name := filepath.Base(filepath.Clean(genDir))

	raw, err := os.ReadFile(filepath.Join(genDir, ManifestFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s (generation not published)", ManifestFilename)
		}
		return nil, fmt.Errorf("reading %s: %w", ManifestFilename, err)
	}

	// 途中で切れた manifest はここで落ちる（閉じ括弧を欠く / ゼロ埋め）。
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", ManifestFilename, err)
	}

	if m.ManifestVersion <= 0 {
		return nil, fmt.Errorf("manifestVersion missing or zero")
	}
	if m.ManifestVersion > ManifestVersion {
		return nil, fmt.Errorf("manifestVersion %d is newer than supported %d",
			m.ManifestVersion, ManifestVersion)
	}
	if m.SchemaVersion <= 0 {
		return nil, fmt.Errorf("schemaVersion missing or zero")
	}
	if m.SchemaVersion > Version {
		return nil, fmt.Errorf("schemaVersion %d is newer than supported %d",
			m.SchemaVersion, Version)
	}
	if m.Generation != name {
		return nil, fmt.Errorf("manifest generation %q does not match directory %q", m.Generation, name)
	}
	if m.Document == "" {
		return nil, fmt.Errorf("manifest has no document")
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("manifest lists no files")
	}

	var documentListed bool
	for _, f := range m.Files {
		if err := validManifestFilename(f.Name); err != nil {
			return nil, err
		}
		if f.Name == m.Document {
			documentListed = true
		}
		if err := verifyFile(filepath.Join(genDir, f.Name), f); err != nil {
			return nil, err
		}
	}
	if !documentListed {
		return nil, fmt.Errorf("document %q is not listed in files", m.Document)
	}
	return &m, nil
}

// validManifestFilename は manifest に載るファイル名が世代ディレクトリ直下の
// 単純名であることを確認する。ディレクトリを跨ぐ名前を検証対象にしない
// （壊れた / 細工された manifest に世代の外のファイルを指させない）。
func validManifestFilename(name string) error {
	if name == "" {
		return fmt.Errorf("manifest lists a file with empty name")
	}
	if name == ManifestFilename {
		// manifest 自身の checksum は自己参照になるので載せられない。
		return fmt.Errorf("manifest must not list itself")
	}
	if name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("manifest lists a non-plain file name %q", name)
	}
	return nil
}

func verifyFile(path string, want ManifestFile) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing file %q listed in manifest", want.Name)
		}
		return fmt.Errorf("opening %q: %w", want.Name, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("hashing %q: %w", want.Name, err)
	}
	if n != want.SizeBytes {
		return fmt.Errorf("size mismatch for %q: on disk %d, manifest %d", want.Name, n, want.SizeBytes)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != want.SHA256 {
		return fmt.Errorf("sha256 mismatch for %q: on disk %s, manifest %s", want.Name, sum, want.SHA256)
	}
	return nil
}
