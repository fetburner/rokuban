package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// LoopMarkers は loop_success_markers を MySQL で操作する。
type LoopMarkers struct {
	db *sql.DB
}

// NewLoopMarkers は MySQL を使う成功マーカー store を作る。
func NewLoopMarkers(db *sql.DB) *LoopMarkers {
	return &LoopMarkers{db: db}
}

// MarkRulerPasses は成功した複数 site の ruler の時刻を 1 つの tx で更新する。
// 検査は TestLoopMarkersContract。
func (s *LoopMarkers) MarkRulerPasses(ctx context.Context, sites []string) error {
	if len(sites) == 0 {
		return nil
	}
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, site := range sites {
			if err := upsertLoopMarker(ctx, tx, store.LoopRuler, site); err != nil {
				return fmt.Errorf("upserting ruler marker for site %q: %w", site, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("marking ruler passes: %w", err)
	}
	return nil
}

// MarkRecordSweep は site の record_sweep 成功時刻を更新する。
func (s *LoopMarkers) MarkRecordSweep(ctx context.Context, site string) error {
	return s.mark(ctx, store.LoopRecordSweep, site)
}

// MarkReconcilePass は pass の全件パスが完走した時刻を保存する。
func (s *LoopMarkers) MarkReconcilePass(ctx context.Context, pass string) error {
	return s.mark(ctx, store.LoopReconcilePass, pass)
}

// MarkDeleteReconcile は置き場 storeName の削除 reconcile が完走した時刻を保存する。
func (s *LoopMarkers) MarkDeleteReconcile(ctx context.Context, storeName string) error {
	return s.mark(ctx, store.LoopDeleteReconcile, storeName)
}

// MarkReconciler は site の予約同期（reconciler）のパスが完走した時刻を保存する。
func (s *LoopMarkers) MarkReconciler(ctx context.Context, site string) error {
	return s.mark(ctx, store.LoopReconciler, site)
}

// MarkEPGSync は site の EPG 全量同期の確定時刻を保存する。
func (s *LoopMarkers) MarkEPGSync(ctx context.Context, site string) error {
	return s.mark(ctx, store.LoopEPGSync, site)
}

// MarkTunerSync は site のチューナー全量同期の確定時刻を保存する。
func (s *LoopMarkers) MarkTunerSync(ctx context.Context, site string) error {
	return s.mark(ctx, store.LoopTunerSync, site)
}

// ReadLoopMarkers は loop の完走時刻を scope ごとに返す。
// 検査は TestLoopMarkersContract。
func (s *LoopMarkers) ReadLoopMarkers(ctx context.Context, loop string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope, last_success_at FROM loop_success_markers WHERE `+"`loop`"+` = ?
	`, loop)
	if err != nil {
		return nil, fmt.Errorf("reading %s markers: %w", loop, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]time.Time)
	for rows.Next() {
		var scope string
		var at time.Time
		if err := rows.Scan(&scope, &at); err != nil {
			return nil, fmt.Errorf("scanning %s marker: %w", loop, err)
		}
		out[scope] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s markers: %w", loop, err)
	}
	return out, nil
}

func (s *LoopMarkers) mark(ctx context.Context, loop, scope string) error {
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		return upsertLoopMarker(ctx, tx, loop, scope)
	})
	if err != nil {
		return fmt.Errorf("upserting %s marker for %q: %w", loop, scope, err)
	}
	return nil
}

// upsertLoopMarker は時刻を DB の NOW(6) で書く。NOW() は文ごとに評価されるので、
// 同じ tx の複数の印が同じ時刻になるとは限らない（pgstore の now() は tx で 1 つ）。契約は同じ時刻を要求しない。
func upsertLoopMarker(ctx context.Context, tx *sql.Tx, loop, scope string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO loop_success_markers (`+"`loop`"+`, scope, last_success_at)
		VALUES (?, ?, NOW(6))
		ON DUPLICATE KEY UPDATE last_success_at = NOW(6)
	`, loop, scope)
	return err
}

var _ store.LoopMarkers = (*LoopMarkers)(nil)
