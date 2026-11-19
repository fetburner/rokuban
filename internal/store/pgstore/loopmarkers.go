package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// MarkRulerPasses は成功した複数 site の ruler の時刻を 1 つのトランザクションで更新する。
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
		if err := q.UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopRuler, Scope: site}); err != nil {
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
	if err := sqlcgen.New(s.pool).UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopRecordSweep, Scope: site}); err != nil {
		return fmt.Errorf("upserting record sweep marker for site %q: %w", site, mapDatabaseError(err))
	}
	return nil
}

// MarkReconcilePass は pass の全件パスが完走した時刻を保存する。
func (s *Reservations) MarkReconcilePass(ctx context.Context, pass string) error {
	if err := sqlcgen.New(s.pool).UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopReconcilePass, Scope: pass}); err != nil {
		return fmt.Errorf("upserting reconcile pass marker %q: %w", pass, mapDatabaseError(err))
	}
	return nil
}

// MarkDeleteReconcile は置き場 storeName の削除 reconcile が完走した時刻を保存する。
func (s *Reservations) MarkDeleteReconcile(ctx context.Context, storeName string) error {
	if err := sqlcgen.New(s.pool).UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopDeleteReconcile, Scope: storeName}); err != nil {
		return fmt.Errorf("upserting delete reconcile marker for store %q: %w", storeName, mapDatabaseError(err))
	}
	return nil
}

// MarkReconciler は site の予約同期（reconciler）のパスが完走した時刻を保存する。
func (s *Reservations) MarkReconciler(ctx context.Context, site string) error {
	if err := sqlcgen.New(s.pool).UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopReconciler, Scope: site}); err != nil {
		return fmt.Errorf("upserting reconciler marker for site %q: %w", site, mapDatabaseError(err))
	}
	return nil
}

// MarkEPGSync は site の EPG 全量同期の確定時刻を保存する。
func (s *Reservations) MarkEPGSync(ctx context.Context, site string) error {
	if err := sqlcgen.New(s.pool).UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopEPGSync, Scope: site}); err != nil {
		return fmt.Errorf("upserting epg sync marker for site %q: %w", site, mapDatabaseError(err))
	}
	return nil
}

// MarkTunerSync は site のチューナー全量同期の確定時刻を保存する。
func (s *Reservations) MarkTunerSync(ctx context.Context, site string) error {
	if err := sqlcgen.New(s.pool).UpsertLoopSuccessMarker(ctx, sqlcgen.UpsertLoopSuccessMarkerParams{Loop: store.LoopTunerSync, Scope: site}); err != nil {
		return fmt.Errorf("upserting tuner sync marker for site %q: %w", site, mapDatabaseError(err))
	}
	return nil
}

// ReadLoopMarkers は loop の完走時刻を scope ごとに返す。
func (s *Reservations) ReadLoopMarkers(ctx context.Context, loop string) (map[string]time.Time, error) {
	rows, err := sqlcgen.New(s.pool).ListLoopSuccessMarkers(ctx, loop)
	if err != nil {
		return nil, fmt.Errorf("reading %s markers: %w", loop, err)
	}
	out := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		out[row.Scope] = row.LastSuccessAt
	}
	return out, nil
}

var _ store.LoopMarkers = (*Reservations)(nil)
