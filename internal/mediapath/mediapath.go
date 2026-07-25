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
