package worker

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// VerifyMediaStoreMarker は media_dir が設定された置き場を指していることを確認する。
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

func requireMediaStoreMarker(mediaDir, storeName, operation string) error {
	if err := VerifyMediaStoreMarker(mediaDir, storeName); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}
