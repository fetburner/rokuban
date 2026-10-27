// Package mediapath はメディアディレクトリ配下の相対パスの解決と検証を行う。
//
// media_assets.rel_path は mirakc が返す contentPath 由来で（M1-5-2）、
// 番組名から生成されるため `..` や絶対パスが混じりうる。書き込み側（ingest）と
// 読み出し側（streamer）の両方で独立に検証する。DB に不正な行が入った場合でも
// 配信側が任意ファイルを読み出さないようにするため、片方だけでは足りない。
package mediapath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscapesMediaDir は相対パスがメディアディレクトリの外を指す場合のエラー。
var ErrEscapesMediaDir = errors.New("path escapes the media directory")

// ErrReservedMediaPath は rel_path がアプリ内部の予約領域を指す場合のエラー。
var ErrReservedMediaPath = errors.New("path uses a reserved media namespace")

// ErrStoreMarkerMismatch はメディア置き場の目印が期待した名前と一致しない場合のエラー。
var ErrStoreMarkerMismatch = errors.New("media store marker mismatch")

// StoreMarkerFileName はメディア置き場の root に置く固定の目印ファイル名。
const StoreMarkerFileName = ".rokuban-store"

// IngestTempFilePrefix は ingest が canonical file と同じディレクトリに作る、record
// 固有一時ファイルの予約接頭辞。孤児回収はこのファイルを通常ファイルとして拾い、
// rescue の catalog 無し走査だけが原本へ昇格させない。
const IngestTempFilePrefix = ".rokuban-ingest-"

// GeneratedAssetTempFilePrefix は生成アセットを公開する前に canonical と同じ
// ディレクトリへ作る staged file の予約接頭辞。孤児回収の候補には残すが、
// catalog 無し rescue では原本へ昇格させない。
const GeneratedAssetTempFilePrefix = ".rokuban-media-asset-"

// MediaRelPathLockDirName は media root 内の rel_path lock 専用ディレクトリ。
// canonical media と同じディレクトリ entry を共有せず、orphan walker は配下全体を飛ばす。
const MediaRelPathLockDirName = ".rokuban-locks"

// MediaRelPathLockFilePrefix は rel_path 固有のファイル排他用 lock file の予約接頭辞。
// 新しい lock file は MediaRelPathLockDirName に置く。旧バージョンの canonical 隣接
// lock file を contentPath として採用しないため、接頭辞は予約し続ける。
const MediaRelPathLockFilePrefix = ".rokuban-rel-path-lock-"

// EncodeTempFilePrefix は encode が canonical file と同じディレクトリに作る、
// 公開前の一時ファイル（scratch からストリームコピーした staged 出力）の予約接頭辞。
// catalog 無し rescue では原本へ昇格させない（拡張子を付けないので rescue の対象に
// ならない）。プロセス死で残った temp は、walkMediaFiles が lock directory、旧形式の
// rel_path lock file、catalog ディレクトリを除いて走査するため孤児候補になり、
// defaultOrphanMTimeGrace
// （7 日）の後に deleteOrphanFile が canonical と同じ手順で消す。
const EncodeTempFilePrefix = ".rokuban-encode-"

// ValidStoreName は置き場名が小文字 ASCII・数字・ハイフンだけで構成されるかを返す。
func ValidStoreName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

// CheckStoreMarker は mediaDir の目印が expected と一致することを確認する。
// expected が空なら、従来構成との互換性のため検査しない。
func CheckStoreMarker(mediaDir, expected string) error {
	if expected == "" {
		return nil
	}
	markerPath := filepath.Join(mediaDir, StoreMarkerFileName)
	info, err := os.Lstat(markerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s is missing", ErrStoreMarkerMismatch, markerPath)
		}
		return fmt.Errorf("stating store marker %q: %w", markerPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrStoreMarkerMismatch, markerPath)
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("reading store marker %q: %w", markerPath, err)
	}
	if string(contents) != expected {
		return fmt.Errorf("%w: expected %q, found %q", ErrStoreMarkerMismatch, expected, contents)
	}
	return nil
}

// RequireStoreMarker は name が有効な置き場名で、mediaDir の目印と一致することを確認する。
// CheckStoreMarker と違い、空の name は検査省略ではなくエラーにする（置き場を決めずに書かせない）。
func RequireStoreMarker(mediaDir, name string) error {
	if !ValidStoreName(name) {
		return fmt.Errorf("invalid media store name %q: set storage.media_store_name", name)
	}
	return CheckStoreMarker(mediaDir, name)
}

// ReadStoreMarker は mediaDir の置き場名を返す。
func ReadStoreMarker(mediaDir string) (string, error) {
	markerPath := filepath.Join(mediaDir, StoreMarkerFileName)
	info, err := os.Lstat(markerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s is missing", ErrStoreMarkerMismatch, markerPath)
		}
		return "", fmt.Errorf("stating store marker %q: %w", markerPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrStoreMarkerMismatch, markerPath)
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return "", fmt.Errorf("reading store marker %q: %w", markerPath, err)
	}
	name := string(contents)
	if !ValidStoreName(name) {
		return "", fmt.Errorf("%w: %s contains invalid store name %q", ErrStoreMarkerMismatch, markerPath, name)
	}
	return name, nil
}

// WriteStoreMarker は marker が無い mediaDir の root に expected を書き、ファイルと root を同期する。
// 既存の同名 marker は成功とし、別名の marker は置き場の改名になるため上書きしない。
func WriteStoreMarker(mediaDir, expected string) error {
	if !ValidStoreName(expected) {
		return fmt.Errorf("invalid media store name %q: use lowercase ASCII letters, digits, and hyphens", expected)
	}
	markerPath := filepath.Join(mediaDir, StoreMarkerFileName)
	file, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		if err := CheckStoreMarker(mediaDir, expected); err != nil {
			return fmt.Errorf("refusing to replace existing store marker: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening store marker %q: %w", markerPath, err)
	}
	if n, writeErr := file.Write([]byte(expected)); writeErr != nil {
		_ = file.Close()
		return fmt.Errorf("writing store marker %q: %w", markerPath, writeErr)
	} else if n != len(expected) {
		_ = file.Close()
		return fmt.Errorf("writing store marker %q: short write (%d of %d bytes)", markerPath, n, len(expected))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("syncing store marker %q: %w", markerPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing store marker %q: %w", markerPath, err)
	}
	dir, err := os.Open(mediaDir)
	if err != nil {
		return fmt.Errorf("opening media directory %q: %w", mediaDir, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("syncing media directory %q: %w", mediaDir, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("closing media directory %q: %w", mediaDir, err)
	}
	return nil
}

// IsIngestTempFile はファイル名が ingest の record 固有一時ファイルかを返す。
// パス全体ではなく basename だけを見るため、走査側と ingest 側で同じ判定を使える。
func IsIngestTempFile(name string) bool {
	return strings.HasPrefix(filepath.Base(name), IngestTempFilePrefix)
}

// IsGeneratedAssetTempFile は生成アセットの公開前 staged file かを返す。
func IsGeneratedAssetTempFile(name string) bool {
	return strings.HasPrefix(filepath.Base(name), GeneratedAssetTempFilePrefix)
}

// IsEncodeTempFile はファイル名が encode の公開前一時ファイルかを返す。
func IsEncodeTempFile(name string) bool {
	return strings.HasPrefix(filepath.Base(name), EncodeTempFilePrefix)
}

// IsMediaRelPathLockFile は rel_path 固有のファイル排他用 lock file かを返す。
func IsMediaRelPathLockFile(name string) bool {
	return strings.HasPrefix(filepath.Base(name), MediaRelPathLockFilePrefix)
}

// Resolve は mediaDir と relPath を結合した絶対パスを返す。
// relPath が mediaDir の外を指す場合は ErrEscapesMediaDir を返す。
// ロックディレクトリ配下と、root の置き場の目印 StoreMarkerFileName は ErrReservedMediaPath を返す。
func Resolve(mediaDir, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("%w: empty path", ErrEscapesMediaDir)
	}

	base := filepath.Clean(mediaDir)
	cleanRelPath := filepath.Clean(relPath)
	if cleanRelPath == MediaRelPathLockDirName || strings.HasPrefix(cleanRelPath, MediaRelPathLockDirName+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrReservedMediaPath, relPath)
	}
	// root の置き場の目印は、media path として unlink・上書きさせない。ネストした同名ファイルは通常のパス。
	if cleanRelPath == StoreMarkerFileName {
		return "", fmt.Errorf("%w: %q", ErrReservedMediaPath, relPath)
	}
	target := filepath.Clean(filepath.Join(base, cleanRelPath))

	// mediaDir 自身を指すのも不可（ファイルでなくディレクトリなので）。
	// 接尾にセパレータを付けて比較することで、
	// /mnt/media-other のような接頭辞一致のすり抜けも防ぐ。
	if !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrEscapesMediaDir, relPath)
	}
	return target, nil
}

// SubtitleSibling は encoded アセットに隣接する WebVTT サイドカーのパスを返す。
// 字幕は独立した media_assets 行を持たず、encoded アセットと同じ basename の
// .vtt として管理する（issue #430 の永続化案 b）。
//
// rel_path（worker のコミット・streamer の配信・削除 reconcile の孤児判定）と
// scratch の絶対パス（encode の ffmpeg 出力先）の両方がこの規則を共有する。
// 3 箇所で別々に拡張子を付け替えていると、片方だけ変えたときに「生成した
// ファイルを誰も配れない / 消せない」形でずれる。
func SubtitleSibling(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	ext := filepath.Ext(path)
	if ext == "" {
		return "", fmt.Errorf("path has no extension: %q", path)
	}
	return strings.TrimSuffix(path, ext) + ".vtt", nil
}
