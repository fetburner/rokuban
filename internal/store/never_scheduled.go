package store

import (
	"context"
	"time"
)

// NeverScheduledEvents は、番組終了後に schedule が観測されなかった事実を記録・削除する契約である。
type NeverScheduledEvents interface {
	// RecordNeverScheduled は site・放送イベント・番組開始時刻の欠測を記録し、新しい行を作ったとき true を返す。
	// 同じ組の再記録は行を作らず false を返す（冪等）。検査: contracttest.RunNeverScheduled。
	RecordNeverScheduled(ctx context.Context, site string, event BroadcastEventKey, programStartAt time.Time) (bool, error)
	// PruneNeverScheduled は DB の時計で horizon より古い欠測観測を削除し、削除行数を返す。
	// 検査: contracttest.RunNeverScheduled。
	PruneNeverScheduled(ctx context.Context, horizon time.Duration) (int64, error)
}
