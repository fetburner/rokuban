package store

import (
	"context"
	"time"
)

// LoopMarkerState は ruler と record_sweep の最後の成功時刻を表す。
// 該当するパスがまだ成功していない時刻は nil である。
type LoopMarkerState struct {
	RulerLastSuccessAt       *time.Time
	RecordSweepLastSuccessAt *time.Time
}

// LoopMarkers はループの成功時刻を記録し、監視向けに読み取る契約である。
type LoopMarkers interface {
	// MarkRulerPasses は成功した複数 site の marker を 1 つのトランザクションで更新する。
	MarkRulerPasses(ctx context.Context, sites []string) error
	// MarkRecordSweep は site の record_sweep 成功時刻を更新する。
	MarkRecordSweep(ctx context.Context, site string) error
	// ReadLoopMarkers は site の marker を返す。未作成の marker は nil の時刻で表す。
	ReadLoopMarkers(ctx context.Context, site string) (LoopMarkerState, error)
}
