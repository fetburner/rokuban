package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ManifestVersion は manifest 自体の形式版。manifest の読み方を壊す変更で上げる
// （catalog document の版 = Version とは別に持つ）。
//
// issue #441 で `files[]`（配列）を単一の `sizeBytes` / `sha256` に畳んだのは
// schemaVersion の「安全側に倒れる引っ越しなら上げない」方針
// （docs/storage/rescue.md §世代の完成判定）を**適用した結果として**上げる。
// この引っ越しは安全側に倒れない —— 旧バイナリが新形式の manifest を読むと
// `files` キーが無いので `len(m.Files) == 0` で世代ごと不完全判定になり、
// 完成世代がまるごと 1 本消える（recordingPurgeRequests のような「要求が
// 落ちても猶予超過で拾われる」側とは違い、この世代の catalog が丸ごと読めなく
// なる）。version を上げれば、この不完全判定が「manifestVersion が新しすぎる」
// という理由の付いた明示的な拒否になる。痛むのは常に「新しい manifest を古い
// バイナリが読む」方向で、逆（旧 manifest を新バイナリが読む）ではない ——
// 運用開始前で旧形式の manifest が本番に存在しないのは「上げる代償が無い」
// 根拠であって「上げなくてよい」根拠ではない。
const ManifestVersion = 2

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
// 存在するだけでは完成を意味しない: VerifyGeneration が本体をサイズと sha256 で
// 照合して初めて完成世代になる（docs/storage.md §8）。
//
// **1 世代 = 本体 1 ファイルだけ**（catalog.json）。かつては複数ファイル世代を
// 見越して Files []ManifestFile を持っていたが、書き手・呼び手が最後まで
// 1 要素しか積まなかったので issue #441 で単一の SizeBytes/SHA256 に畳んだ。
// 複数ファイル世代が要るようになったら、そのときの書き手と同じ PR で
// 形を決め直す（不変条件 11）。
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
	// Document は本体ファイル名。DocumentFilename と一致しなければ不完全に倒す。
	Document string `json:"document"`
	// SizeBytes は本体ファイルのサイズ。
	SizeBytes int64 `json:"sizeBytes"`
	// SHA256 は本体ファイルの sha256（16 進）。
	SHA256 string `json:"sha256"`
}

// VerifyGeneration は世代ディレクトリが完成世代かを判定し、manifest を返す。
//
// 完成条件（docs/storage.md §8「世代の完成判定」。1 つでも欠ければ error）:
//
//  1. manifest.json が存在し、JSON として最後まで parse できる
//  2. manifestVersion がこのバイナリの理解する版以下
//  3. schemaVersion がこのバイナリの読める catalog 版以下
//  4. generation が世代ディレクトリ名と一致する
//  5. document が DocumentFilename と一致し、本体ファイルが存在してサイズと
//     sha256 が両方一致する
//
// rename のアトミック性には依存しない。判定の材料は世代ディレクトリの中身だけ。
//
// 本体のパスはここでも SelectLatest（write.go）でも常に DocumentFilename
// （定数）から組み立てる。manifest.Document フィールドの値を使ってパスを
// 組み立てることはしない。この安全性は**型で強制されてはいない**が、独立した
// 2 層で守っている: (1) 上の等値検査（判定 5）が「document は
// DocumentFilename と一致しなければ不完全」を保証する、(2) パスを組み立てる
// 側が manifest.Document を一切参照せず常に定数から組み立てる。判定 5 を
// 経由しない呼び出し（selectFromStatuses に直接 Manifest を渡す
// TestSelectFromStatuses_DocumentPathIgnoresManifestDocument）でも (2) だけで
// パス走査を防げること、逆に将来 (2) が崩れても正規の経路では (1) が
// manifest.Document を定数に固定していること、の両方が効く。
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
	if m.Document != DocumentFilename {
		return nil, fmt.Errorf("manifest document %q does not match expected %q", m.Document, DocumentFilename)
	}
	if err := verifyFile(filepath.Join(genDir, DocumentFilename), m.SizeBytes, m.SHA256); err != nil {
		return nil, err
	}
	return &m, nil
}

// verifyFile は path の内容をサイズと sha256 で照合する。この文字列は
// SnapshotStatus.Reason 経由で `rokuban catalog verify` の出力にそのまま出る。
//
// missing / size mismatch / sha256 mismatch の 3 分岐は path の basename
// だけを出す（path は media_dir 配下の絶対パスであり、運用者の目に漏らさない
// ため）。**opening / hashing の 2 分岐はそうなっていない** —— `%w` で
// *fs.PathError をそのまま wrap しており、その Error() が path を丸ごと含む
// ので絶対パスが出る。os.Open や io.Copy の失敗は稀な I/O エラーで、
// 診断のため元エラーの情報を落とさない方を優先している（%q で name を
// 重ねて出すのは *fs.PathError と重複するだけなので、そちらは付けない）。
func verifyFile(path string, wantSize int64, wantSHA256 string) error {
	name := filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing document file %q", name)
		}
		return fmt.Errorf("opening document: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("hashing document: %w", err)
	}
	if n != wantSize {
		return fmt.Errorf("size mismatch for %q: on disk %d, manifest %d", name, n, wantSize)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != wantSHA256 {
		return fmt.Errorf("sha256 mismatch for %q: on disk %s, manifest %s", name, sum, wantSHA256)
	}
	return nil
}
