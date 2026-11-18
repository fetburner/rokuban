// Package storemarker は置き場の印を照合し、不一致を計測する。
// 起動時と worker の操作前で同じ照合と計測を使うため、
// mediapath の照合を呼ぶこのパッケージに計測をまとめる。
package storemarker

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// VerifyMediaStoreMarker は media_dir が設定された置き場を指していることを確認する。
// 起動時の検査と worker の操作前の検査で、照合と不一致の計測を共有する。
// 置き場名が空なら既存構成との互換性のため検査しない。
func VerifyMediaStoreMarker(mediaDir, storeName string) error {
	if storeName == "" {
		return nil
	}
	if err := mediapath.CheckStoreMarker(mediaDir, storeName); err != nil {
		metrics.StoreMarkerMismatch.WithLabelValues(storeName).Inc()
		return fmt.Errorf("verifying media store %q marker: %w", storeName, err)
	}
	return nil
}

// RequireMediaStoreMarker は操作名を付けて置き場の照合エラーを返す。
// 生成・転送・削除・取り込みはファイル操作や DB 更新の前に同じ照合を使う。
func RequireMediaStoreMarker(mediaDir, storeName, operation string) error {
	if err := VerifyMediaStoreMarker(mediaDir, storeName); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}
