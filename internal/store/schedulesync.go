package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
)

// BroadcastEventKey は放送イベントを一意にする外部キーである。
type BroadcastEventKey struct {
	NetworkID int32
	ServiceID int32
	EventID   int32
}

// ObservedSchedule は mirakc から観測した schedule の永続化・読み取り形である。
type ObservedSchedule struct {
	ProgramID    int64
	State        string
	Options      json.RawMessage
	Tags         []string
	FailedReason json.RawMessage
}

// ScheduleSync は reconciler が schedule の観測と予約との差分を永続化・読む契約である。
type ScheduleSync interface {
	// ReplaceObserved は site の観測行を observed で置き換え、古い観測行を削除し、snapshot marker を更新する。
	// この三つを 1 tx で原子的に確定する。古い観測行の判定時刻は DB の時計で付け、呼び出し側は時刻を渡さない（契約テストでは未検証）。
	// 検査: contracttest.RunScheduleSync の「replace removes stale observations and advances marker」「failed replacement preserves prior observations and marker」「marker failure rolls back observations」「replacement is scoped to one site」。
	ReplaceObserved(ctx context.Context, site string, observed []ObservedSchedule) error
	ListForSyncEvaluation(ctx context.Context, site string) ([]reservation.SyncEvaluationRow, error)
	ListStartedBroadcastEventKeys(ctx context.Context, site string, keys []BroadcastEventKey, startedAfter time.Time) ([]BroadcastEventKey, error)
}

// ScheduleSyncReader は presync collector が snapshot marker と desired/observed を読む契約である。
type ScheduleSyncReader interface {
	// SnapshotAt は site の snapshot marker を返す。marker が無いときは、エラーではなく found=false を返す（契約テストでは未検証）。
	SnapshotAt(ctx context.Context, site string) (time.Time, bool, error)
	ListObserved(ctx context.Context, site string) ([]ObservedSchedule, error)
	ListForSyncEvaluation(ctx context.Context, site string) ([]reservation.SyncEvaluationRow, error)
}
