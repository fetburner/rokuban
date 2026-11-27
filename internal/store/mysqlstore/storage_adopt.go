package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/fetburner/rokuban/internal/storagename"
)

// AdoptUnassignedStoreRows は範囲が空の delete_reconcile ブレーカーをこの置き場へ割り当てる。
// コピー行は作らない。PostgreSQL の pgstore.Media.AdoptUnassignedStoreRows と同じ手順である。
// 既にこの置き場のブレーカーがあれば残す（ON DUPLICATE KEY で何もしない）。
func (s *Media) AdoptUnassignedStoreRows(ctx context.Context) error {
	if !storagename.ValidStoreName(s.storeName) {
		return fmt.Errorf("invalid media store name %q", s.storeName)
	}
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO circuit_breakers (scope, name, tripped_at, pending, threshold, detail)
			SELECT ?, src.name, src.tripped_at, src.pending, src.threshold, src.detail
			FROM circuit_breakers AS src
			WHERE src.scope = '' AND src.name = 'delete_reconcile'
			ON DUPLICATE KEY UPDATE name = circuit_breakers.name`, s.storeName); err != nil {
			return fmt.Errorf("assigning unowned delete_reconcile breaker: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM circuit_breakers WHERE scope = '' AND name = 'delete_reconcile'`); err != nil {
			return fmt.Errorf("removing unowned delete_reconcile breaker: %w", err)
		}
		return nil
	})
}

// HasStoreCopies はこの置き場のコピー行が DB に 1 件でもあるかを返す。
// storage adopt が、目印の無い root に目印を新しく作ってよいかの判断に使う。
func (s *Media) HasStoreCopies(ctx context.Context) (bool, error) {
	var has bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM media_asset_copies WHERE store = ?)`, s.storeName).Scan(&has); err != nil {
		return false, fmt.Errorf("checking media asset copies in store %q: %w", s.storeName, err)
	}
	return has, nil
}
