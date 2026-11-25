package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

// GetRulerInputRevision は site の入力版を返す。行が無ければ 0 を返す。
func (s *Reservations) GetRulerInputRevision(ctx context.Context, site string) (int64, error) {
	var revision int64
	err := s.db.QueryRowContext(ctx, `
 SELECT COALESCE((SELECT revision FROM ruler_input_revisions WHERE site = ?), 0)
 `, site).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("reading ruler input revision for %q: %w", site, err)
	}
	return revision, nil
}

// BumpRulerInputRevision は site の ruler 入力版を 1 進める。
func (s *Reservations) BumpRulerInputRevision(ctx context.Context, site string) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error { return bumpRulerInputRevision(ctx, tx, site) })
}

// bumpRulerInputRevision は入力を書いた tx の中で site の版を進める。
func bumpRulerInputRevision(ctx context.Context, tx *sql.Tx, site string) error {
	if _, err := tx.ExecContext(ctx, `
 INSERT INTO ruler_input_revisions (site, revision) VALUES (?, 1)
 ON DUPLICATE KEY UPDATE revision = revision + 1
 `, site); err != nil {
		return fmt.Errorf("bumping ruler input revision for %q: %w", site, err)
	}
	return nil
}

func lockRulerInputRevision(ctx context.Context, tx *sql.Tx, site string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
 INSERT INTO ruler_input_revisions (site, revision) VALUES (?, 0)
 ON DUPLICATE KEY UPDATE revision = revision
 `, site); err != nil {
		return 0, fmt.Errorf("ensuring input revision row: %w", err)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `
 SELECT revision FROM ruler_input_revisions WHERE site = ? FOR UPDATE
 `, site).Scan(&revision); err != nil {
		return 0, fmt.Errorf("locking input revision row: %w", err)
	}
	return revision, nil
}

var _ store.RulerInputBumper = (*Reservations)(nil)
