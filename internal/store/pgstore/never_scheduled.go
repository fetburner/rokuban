package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// NeverScheduledEvents は never_scheduled_events を PostgreSQL で操作する。
type NeverScheduledEvents struct {
	pool *pgxpool.Pool
}

// NewNeverScheduledEvents は PostgreSQL を使う欠測観測 store を作る。
func NewNeverScheduledEvents(pool *pgxpool.Pool) *NeverScheduledEvents {
	return &NeverScheduledEvents{pool: pool}
}

// RecordNeverScheduled は放送イベントの欠測を記録し、新しい行を作ったとき true を返す。
func (s *NeverScheduledEvents) RecordNeverScheduled(ctx context.Context, site string, event store.BroadcastEventKey) (bool, error) {
	rows, err := sqlcgen.New(s.pool).CreateNeverScheduledEvent(ctx, sqlcgen.CreateNeverScheduledEventParams{
		Site: site, NetworkID: event.NetworkID, ServiceID: event.ServiceID, EventID: event.EventID,
	})
	if err != nil {
		return false, fmt.Errorf("recording never-scheduled event %s/%d/%d/%d: %w", site, event.NetworkID, event.ServiceID, event.EventID, err)
	}
	return rows > 0, nil
}

// PruneNeverScheduled は horizon より古い欠測観測を削除する。
//
// observed_at を DB の時計で書いているので、比べる時刻も DB の now() で作る。
// 呼び出し側は幅だけ渡す。
func (s *NeverScheduledEvents) PruneNeverScheduled(ctx context.Context, horizon time.Duration) (int64, error) {
	rows, err := sqlcgen.New(s.pool).DeleteStaleNeverScheduledEvents(ctx, pgtype.Interval{Microseconds: horizon.Microseconds(), Valid: true})
	if err != nil {
		return 0, fmt.Errorf("pruning never-scheduled events: %w", err)
	}
	return rows, nil
}

var _ store.NeverScheduledEvents = (*NeverScheduledEvents)(nil)
