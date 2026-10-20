package store

import (
	"context"
	"time"
)

// NeverScheduledEvents は、番組終了後に schedule が観測されなかった事実を記録・削除する契約である。
type NeverScheduledEvents interface {
	// RecordNeverScheduled は放送イベントの欠測を記録し、新しい行を作ったとき true を返す。
	RecordNeverScheduled(ctx context.Context, site string, event BroadcastEventKey) (bool, error)
	// PruneNeverScheduled は DB の時計で horizon より古い欠測観測を削除し、削除行数を返す。
	PruneNeverScheduled(ctx context.Context, horizon time.Duration) (int64, error)
}
