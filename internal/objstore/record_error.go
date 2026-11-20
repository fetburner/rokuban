package objstore

import (
	"errors"

	"github.com/fetburner/rokuban/internal/metrics"
)

// RecordError は object store の操作の失敗を数える。
// 資格情報の失効・取得失敗は rokuban_object_store_credential_errors_total にも数え、権限や接続の失敗と区別する。
//
// 資格情報のエラーの判定（ErrCredentials）はこのパッケージが持つので、ここに置く。
// 計器の定義を持つ metrics は object store を知らないまま保つ。
func RecordError(operation string, err error) {
	metrics.ObjectStoreErrors.WithLabelValues(operation).Inc()
	if errors.Is(err, ErrCredentials) {
		metrics.ObjectStoreCredentialErrors.Inc()
	}
}
