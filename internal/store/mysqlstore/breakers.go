package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// Breakers は MySQL でサーキットブレーカーの契約を実装する。
type Breakers struct {
	db *sql.DB
}

// NewBreakers は MySQL を使うブレーカー store を作る。
func NewBreakers(db *sql.DB) *Breakers {
	return &Breakers{db: db}
}

// Trip はブレーカーの発動を記録し、発動中なら最初の発動時刻を保つ。
// 検査は TestBreakersContract。
func (s *Breakers) Trip(ctx context.Context, scope, name string, threshold int, sample store.BreakerSample) (time.Time, error) {
	var trippedAt time.Time
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		trippedAt, err = tripCircuitBreaker(ctx, tx, scope, name, threshold, sample, nil)
		return err
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("tripping circuit breaker %q: %w", name, err)
	}
	return trippedAt, nil
}

// tripCircuitBreaker は呼び出し元の書き込みと同じ tx で発動を記録する。
func tripCircuitBreaker(ctx context.Context, tx *sql.Tx, scope, name string, threshold int, sample store.BreakerSample, now *time.Time) (time.Time, error) {
	detail, err := json.Marshal(store.TruncateBreakerSample(sample))
	if err != nil {
		return time.Time{}, fmt.Errorf("marshalling circuit breaker sample: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
  INSERT INTO circuit_breakers (scope, name, pending, threshold, detail, tripped_at)
  VALUES (?, ?, ?, ?, CAST(? AS JSON), COALESCE(?, CURRENT_TIMESTAMP(6))) AS incoming
  ON DUPLICATE KEY UPDATE
   pending = incoming.pending,
   threshold = incoming.threshold,
   detail = incoming.detail
 `, scope, name, sample.Total, threshold, string(detail), now); err != nil {
		return time.Time{}, fmt.Errorf("writing circuit breaker %q: %w", name, err)
	}
	var trippedAt time.Time
	if err := tx.QueryRowContext(ctx, `
  SELECT tripped_at FROM circuit_breakers WHERE scope = ? AND name = ?
 `, scope, name).Scan(&trippedAt); err != nil {
		return time.Time{}, fmt.Errorf("reading tripped time for circuit breaker %q: %w", name, err)
	}
	return trippedAt, nil
}

// IsTripped は指定したブレーカーの発動行の有無を返す。
// 検査は contracttest.RunBreakers。
func (s *Breakers) IsTripped(ctx context.Context, scope, name string) (bool, error) {
	var tripped bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM circuit_breakers WHERE scope = ? AND name = ?)
	`, scope, name).Scan(&tripped); err != nil {
		return false, fmt.Errorf("checking circuit breaker %q: %w", name, err)
	}
	return tripped, nil
}

// List は発動中のブレーカーを scope、name 順で返す。
// 検査は contracttest.RunBreakers。
func (s *Breakers) List(ctx context.Context) ([]store.CircuitBreaker, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope, name, tripped_at, pending, threshold, detail
		FROM circuit_breakers ORDER BY scope, name
	`)
	if err != nil {
		return nil, fmt.Errorf("listing circuit breakers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]store.CircuitBreaker, 0)
	for rows.Next() {
		var row store.CircuitBreaker
		var detail []byte
		if err := rows.Scan(&row.Scope, &row.Name, &row.TrippedAt, &row.Pending, &row.Threshold, &detail); err != nil {
			return nil, fmt.Errorf("scanning circuit breaker: %w", err)
		}
		row.Detail = json.RawMessage(detail)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating circuit breakers: %w", err)
	}
	return result, nil
}

// Resume は発動行を削除し、行が無ければ store.ErrNotFound を返す。
// 検査は contracttest.RunBreakers。
func (s *Breakers) Resume(ctx context.Context, scope, name string) error {
	var affected int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM circuit_breakers WHERE scope = ? AND name = ?`, scope, name)
		if err != nil {
			return fmt.Errorf("deleting circuit breaker %q: %w", name, err)
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting deleted circuit breaker %q: %w", name, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("resuming circuit breaker %q: %w", name, err)
	}
	if affected == 0 {
		return fmt.Errorf("circuit breaker %q at scope %q: %w", name, scope, store.ErrNotFound)
	}
	return nil
}

var _ store.Breakers = (*Breakers)(nil)
var _ store.BreakerAdmin = (*Breakers)(nil)
