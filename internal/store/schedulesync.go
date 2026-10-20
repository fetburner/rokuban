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
// ReplaceObserved は観測行の置換、古い観測の削除、snapshot marker の更新を原子的に確定する。
type ScheduleSync interface {
	ReplaceObserved(ctx context.Context, site string, observed []ObservedSchedule) error
	ListForSyncEvaluation(ctx context.Context, site string) ([]reservation.SyncEvaluationRow, error)
	ListStartedBroadcastEventKeys(ctx context.Context, site string, keys []BroadcastEventKey, startedAfter time.Time) ([]BroadcastEventKey, error)
}

// ScheduleSyncReader は presync collector が snapshot marker と desired/observed を読む契約である。
type ScheduleSyncReader interface {
	SnapshotAt(ctx context.Context, site string) (time.Time, bool, error)
	ListObserved(ctx context.Context, site string) ([]ObservedSchedule, error)
	ListForSyncEvaluation(ctx context.Context, site string) ([]reservation.SyncEvaluationRow, error)
}
