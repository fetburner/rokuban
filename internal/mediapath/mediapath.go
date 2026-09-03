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
	"path/filepath"
	"strings"
)

// ErrEscapesMediaDir は相対パスがメディアディレクトリの外を指す場合のエラー。
var ErrEscapesMediaDir = errors.New("path escapes the media directory")

// Resolve は mediaDir と relPath を結合した絶対パスを返す。
// relPath が mediaDir の外を指す場合は ErrEscapesMediaDir を返す。
func Resolve(mediaDir, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("%w: empty path", ErrEscapesMediaDir)
	}

	base := filepath.Clean(mediaDir)
	target := filepath.Clean(filepath.Join(base, relPath))

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
