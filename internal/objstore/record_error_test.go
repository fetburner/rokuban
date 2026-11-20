package objstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/metrics"
)

// RecordError は資格情報の失敗だけ credential 系列も増やすこと。
// 権限や接続の失敗は操作別の系列だけに数える（両者を区別して見るため）。
func TestRecordErrorCountsCredentialFailuresSeparately(t *testing.T) {
	before := testutil.ToFloat64(metrics.ObjectStoreCredentialErrors)
	beforeOp := testutil.ToFloat64(metrics.ObjectStoreErrors.WithLabelValues("put"))

	RecordError("put", fmt.Errorf("denied: %w", ErrCredentials))
	RecordError("put", errors.New("connection reset"))

	if got := testutil.ToFloat64(metrics.ObjectStoreErrors.WithLabelValues("put")) - beforeOp; got != 2 {
		t.Errorf("object store errors{operation=put} delta = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.ObjectStoreCredentialErrors) - before; got != 1 {
		t.Errorf("credential errors delta = %v, want 1", got)
	}
}
