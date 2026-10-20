package breaker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// Querier は ruler の tx 内でブレーカーを発動する DB 操作を表す。
//
// この例外は ruler の発動記録と同じ tx に TripInTx を含めるためだけに置く。
type Querier interface {
	TripCircuitBreaker(ctx context.Context, arg sqlcgen.TripCircuitBreakerParams) (sqlcgen.CircuitBreaker, error)
}

// TripInTx は ruler の tx 内でブレーカーを発動させる。
//
// 呼び出し側は TripInTx の後、その削除を実行してはならない。記録に失敗した場合も削除しない。
func TripInTx(ctx context.Context, q Querier, site, name string, threshold int, sample Sample) error {
	detail, err := json.Marshal(TruncateSample(sample))
	if err != nil {
		return fmt.Errorf("marshalling circuit breaker sample: %w", err)
	}
	row, err := q.TripCircuitBreaker(ctx, sqlcgen.TripCircuitBreakerParams{
		Site: site, Name: name, Pending: int32(sample.Total), Threshold: int32(threshold), Detail: detail,
	})
	if err != nil {
		return fmt.Errorf("tripping circuit breaker %q: %w", name, err)
	}
	announceTrip(site, name, threshold, sample, row.TrippedAt)
	return nil
}
