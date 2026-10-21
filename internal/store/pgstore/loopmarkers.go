package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// MarkRulerPasses は成功した複数 site の marker を 1 つのトランザクションで更新する。
func (s *Reservations) MarkRulerPasses(ctx context.Context, sites []string) error {
	if len(sites) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning ruler marker transaction: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	for _, site := range sites {
		if err := q.UpsertRulerPassSnapshot(ctx, site); err != nil {
			return fmt.Errorf("upserting ruler marker for site %q: %w", site, mapDatabaseError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing ruler markers: %w", mapDatabaseError(err))
	}
	return nil
}

// MarkRecordSweep は site の record_sweep 成功時刻を更新する。
func (s *Reservations) MarkRecordSweep(ctx context.Context, site string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning record sweep marker transaction: %w", err)
	}
	defer rollback(tx)
	if err := sqlcgen.New(tx).UpsertRecordSweepSnapshot(ctx, site); err != nil {
		return fmt.Errorf("upserting record sweep marker for site %q: %w", site, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing record sweep marker for site %q: %w", site, mapDatabaseError(err))
	}
	return nil
}

// ReadLoopMarkers は site の ruler と record_sweep の成功時刻を返す。
func (s *Reservations) ReadLoopMarkers(ctx context.Context, site string) (store.LoopMarkerState, error) {
	q := sqlcgen.New(s.pool)
	var state store.LoopMarkerState
	rulerAt, err := q.GetRulerPassSnapshot(ctx, site)
	if err == nil {
		state.RulerLastSuccessAt = &rulerAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.LoopMarkerState{}, fmt.Errorf("reading ruler marker for site %q: %w", site, err)
	}
	sweepAt, err := q.GetRecordSweepSnapshot(ctx, site)
	if err == nil {
		state.RecordSweepLastSuccessAt = &sweepAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.LoopMarkerState{}, fmt.Errorf("reading record sweep marker for site %q: %w", site, err)
	}
	return state, nil
}

var _ store.LoopMarkers = (*Reservations)(nil)
