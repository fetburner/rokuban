package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// scheduleSyncBatchSize は 1 回の pgx.Batch に詰める行数。1 バッチあたりのメモリと所要を抑える。
// internal/worker/epg.go の epgBatchSize と揃える。
const scheduleSyncBatchSize = 1000

// ScheduleSync は reconciler の schedule 同期を PostgreSQL で実装する。
type ScheduleSync struct {
	pool *pgxpool.Pool
}

// NewScheduleSync は PostgreSQL を使う schedule 同期 store を作る。
func NewScheduleSync(pool *pgxpool.Pool) *ScheduleSync {
	return &ScheduleSync{pool: pool}
}

// ReplaceObserved は全量観測を置き換え、snapshot marker を同じ tx で進める。
// 新旧の観測と marker は一体で確定し、collector が部分的な snapshot を新鮮と扱わない。
func (s *ScheduleSync) ReplaceObserved(ctx context.Context, site string, observed []store.ObservedSchedule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning schedule snapshot transaction: %w", err)
	}
	defer rollback(tx)

	q := sqlcgen.New(tx)
	sweepTime, err := q.ScheduleSyncSweepMark(ctx)
	if err != nil {
		return fmt.Errorf("getting schedule snapshot mark: %w", err)
	}

	// 空の snapshot ではこのループが回らないので空 batch は送らない。
	// 古い観測行の削除と snapshot marker の更新は、観測が 0 件でも行う。
	for start := 0; start < len(observed); start += scheduleSyncBatchSize {
		end := min(start+scheduleSyncBatchSize, len(observed))
		params := make([]sqlcgen.UpsertScheduleSyncParams, 0, end-start)
		for _, row := range observed[start:end] {
			tags := row.Tags
			if tags == nil {
				tags = []string{}
			}
			params = append(params, sqlcgen.UpsertScheduleSyncParams{
				Site:         site,
				ProgramID:    row.ProgramID,
				State:        row.State,
				Options:      row.Options,
				Tags:         tags,
				FailedReason: row.FailedReason,
			})
		}
		if err := execScheduleSyncBatch(ctx, q, params); err != nil {
			return err
		}
	}

	if err := q.DeleteStaleScheduleSyncs(ctx, sqlcgen.DeleteStaleScheduleSyncsParams{Site: site, ObservedAt: sweepTime}); err != nil {
		return fmt.Errorf("cleaning stale schedule_syncs: %w", err)
	}
	if err := q.UpsertScheduleSyncSnapshot(ctx, site); err != nil {
		return fmt.Errorf("updating schedule snapshot marker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing schedule snapshot: %w", err)
	}
	return nil
}

// ListForSyncEvaluation は同期対象の候補行を返す。
func (s *ScheduleSync) ListForSyncEvaluation(ctx context.Context, site string) ([]reservation.SyncEvaluationRow, error) {
	rows, err := sqlcgen.New(s.pool).ListReservationsForSyncEvaluation(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing reservations for sync evaluation: %w", err)
	}
	result := make([]reservation.SyncEvaluationRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, reservation.SyncEvaluationRow{
			Reservation: reservation.SyncReservation{
				ProgramID: row.Reservation.ProgramID,
				Base:      row.Reservation.Base,
			},
			ProgramSnapshot: reservation.SyncProgramSnapshot{
				Title:       row.ProgramSnapshot.Title,
				StartAt:     row.ProgramSnapshot.StartAt,
				DurationMs:  row.ProgramSnapshot.DurationMs,
				ChannelType: row.ProgramSnapshot.ChannelType,
				Channel:     row.ProgramSnapshot.Channel,
				NetworkID:   row.ProgramSnapshot.NetworkID,
				ServiceID:   row.ProgramSnapshot.ServiceID,
				EventID:     row.ProgramSnapshot.EventID,
			},
			IntentAction: row.IntentAction,
			Overrides:    row.Overrides,
		})
	}
	return result, nil
}

// ListStartedBroadcastEventKeys は開始済み録画がある放送イベントキーを返す。
func (s *ScheduleSync) ListStartedBroadcastEventKeys(ctx context.Context, site string, keys []store.BroadcastEventKey, startedAfter time.Time) ([]store.BroadcastEventKey, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	networkIDs := make([]int32, len(keys))
	serviceIDs := make([]int32, len(keys))
	eventIDs := make([]int32, len(keys))
	for i, key := range keys {
		networkIDs[i] = key.NetworkID
		serviceIDs[i] = key.ServiceID
		eventIDs[i] = key.EventID
	}
	rows, err := sqlcgen.New(s.pool).ListStartedBroadcastEventKeys(ctx, sqlcgen.ListStartedBroadcastEventKeysParams{
		Site: site, NetworkIds: networkIDs, ServiceIds: serviceIDs, EventIds: eventIDs, StartedAfter: startedAfter,
	})
	if err != nil {
		return nil, fmt.Errorf("listing started broadcast event keys: %w", err)
	}
	result := make([]store.BroadcastEventKey, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.BroadcastEventKey{NetworkID: row.NetworkID, ServiceID: row.ServiceID, EventID: row.EventID})
	}
	return result, nil
}

// SnapshotAt は最後に確定した全量 snapshot の時刻を返す。未観測なら exists は false である。
func (s *ScheduleSync) SnapshotAt(ctx context.Context, site string) (time.Time, bool, error) {
	at, err := sqlcgen.New(s.pool).GetScheduleSyncSnapshot(ctx, site)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("getting schedule snapshot marker: %w", err)
	}
	return at, true, nil
}

// ListObserved は 1 site の schedule 観測を program ID 順で返す。
func (s *ScheduleSync) ListObserved(ctx context.Context, site string) ([]store.ObservedSchedule, error) {
	rows, err := sqlcgen.New(s.pool).ListScheduleSyncsBySite(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing schedule observations: %w", err)
	}
	result := make([]store.ObservedSchedule, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.ObservedSchedule{
			ProgramID: row.ProgramID, State: row.State, Options: row.Options, Tags: row.Tags, FailedReason: row.FailedReason,
		})
	}
	return result, nil
}

// execScheduleSyncBatch は 1 バッチ分の schedule_sync を実行し、最初のエラーを program ID 付きで返す。
// Close は成功・失敗どちらの経路でも、呼び出し元の defer の Rollback より前に完了する。
//
// sqlc 生成の :batchexec は範囲外の i で呼ばないので、防御分岐は置かない。
func execScheduleSyncBatch(ctx context.Context, q *sqlcgen.Queries, params []sqlcgen.UpsertScheduleSyncParams) error {
	batch := q.UpsertScheduleSync(ctx, params)
	var batchErr error
	batch.Exec(func(i int, err error) {
		if err == nil || batchErr != nil {
			return
		}
		batchErr = fmt.Errorf("upserting schedule_sync for program %d: %w", params[i].ProgramID, err)
	})
	if closeErr := batch.Close(); closeErr != nil && batchErr == nil {
		batchErr = fmt.Errorf("closing schedule_sync batch: %w", closeErr)
	}
	return batchErr
}

var _ store.ScheduleSync = (*ScheduleSync)(nil)
var _ store.ScheduleSyncReader = (*ScheduleSync)(nil)
