package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// MarkReconcilePass は pass の全件パスが完走した時刻を保存する。
func (s *Reservations) MarkReconcilePass(ctx context.Context, pass string) error {
	if err := sqlcgen.New(s.pool).UpsertReconcilePassSnapshot(ctx, pass); err != nil {
		return fmt.Errorf("upserting reconcile pass marker %q: %w", pass, mapDatabaseError(err))
	}
	return nil
}

// ReadReconcilePassMarkers は完走した全件パスの時刻を pass ごとに返す。
func (s *Reservations) ReadReconcilePassMarkers(ctx context.Context) (map[string]time.Time, error) {
	rows, err := sqlcgen.New(s.pool).ListReconcilePassSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading reconcile pass markers: %w", err)
	}
	out := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		out[row.Pass] = row.LastSuccessAt
	}
	return out, nil
}

// MarkDeleteReconcile は置き場 storeName の削除 reconcile が完走した時刻を保存する。
func (s *Reservations) MarkDeleteReconcile(ctx context.Context, storeName string) error {
	if err := sqlcgen.New(s.pool).UpsertDeleteReconcileSnapshot(ctx, storeName); err != nil {
		return fmt.Errorf("upserting delete reconcile marker for store %q: %w", storeName, mapDatabaseError(err))
	}
	return nil
}

// ReadDeleteReconcileMarkers は完走した置き場の削除 reconcile の時刻を置き場ごとに返す。
func (s *Reservations) ReadDeleteReconcileMarkers(ctx context.Context) (map[string]time.Time, error) {
	rows, err := sqlcgen.New(s.pool).ListDeleteReconcileSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading delete reconcile markers: %w", err)
	}
	out := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		out[row.Store] = row.LastSuccessAt
	}
	return out, nil
}

var (
	_ store.ReconcilePassMarkers   = (*Reservations)(nil)
	_ store.DeleteReconcileMarkers = (*Reservations)(nil)
)
