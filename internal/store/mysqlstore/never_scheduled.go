package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/store"
)

// NeverScheduledEvents は never_scheduled_events を MySQL で操作する。
type NeverScheduledEvents struct {
	db *sql.DB
}

// NewNeverScheduledEvents は MySQL を使う欠測観測 store を作る。
func NewNeverScheduledEvents(db *sql.DB) *NeverScheduledEvents {
	return &NeverScheduledEvents{db: db}
}

// RecordNeverScheduled は放送イベントと番組開始時刻の欠測を記録し、新しい行を作ったとき true を返す。
// 同じ組の重複は文のエラーとして受け、tx を commit して false を返す（MySQL は文の失敗後も tx を続行する）。
// pgstore は生きている recordings 行がある放送イベントを記録しないが、この実装はその条件を持たない（契約に無い。#1458）。
// 検査は TestNeverScheduledEventsContract。
func (s *NeverScheduledEvents) RecordNeverScheduled(ctx context.Context, site string, event store.BroadcastEventKey, programStartAt time.Time) (bool, error) {
	var created bool
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO never_scheduled_events (site, network_id, service_id, event_id, program_start_at)
			VALUES (?, ?, ?, ?, ?)
		`, site, event.NetworkID, event.ServiceID, event.EventID, programStartAt)
		if mysqldb.IsUniqueViolation(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("writing never-scheduled event: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting written never-scheduled events: %w", err)
		}
		created = affected == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("recording never-scheduled event %s/%d/%d/%d at %s: %w", site, event.NetworkID, event.ServiceID, event.EventID, programStartAt.Format(time.RFC3339Nano), err)
	}
	return created, nil
}

// PruneNeverScheduled は DB の時計で horizon より古い欠測観測を削除し、削除行数を返す。
// 比べる時刻は NOW(6) で DB 側に作らせる。ホストの時計を渡すと、observed_at を書いた DB の時計と食い違う。
func (s *NeverScheduledEvents) PruneNeverScheduled(ctx context.Context, horizon time.Duration) (int64, error) {
	var deleted int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM never_scheduled_events
			WHERE observed_at < NOW(6) - INTERVAL ? MICROSECOND
		`, horizon.Microseconds())
		if err != nil {
			return fmt.Errorf("deleting stale never-scheduled events: %w", err)
		}
		deleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting deleted never-scheduled events: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("pruning never-scheduled events: %w", err)
	}
	return deleted, nil
}

var _ store.NeverScheduledEvents = (*NeverScheduledEvents)(nil)
